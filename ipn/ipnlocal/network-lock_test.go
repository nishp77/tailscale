// Copyright (c) 2022 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipnlocal

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"tailscale.com/control/controlclient"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
	"tailscale.com/tka"
	"tailscale.com/types/key"
	"tailscale.com/types/netmap"
	"tailscale.com/types/persist"
	"tailscale.com/types/tkatype"
)

func fakeControlClient(t *testing.T, c *http.Client) *controlclient.Auto {
	hi := hostinfo.New()
	ni := tailcfg.NetInfo{LinkType: "wired"}
	hi.NetInfo = &ni

	k := key.NewMachine()
	opts := controlclient.Options{
		ServerURL: "https://example.com",
		Hostinfo:  hi,
		GetMachinePrivateKey: func() (key.MachinePrivate, error) {
			return k, nil
		},
		HTTPTestClient:  c,
		NoiseTestClient: c,
		Status:          func(controlclient.Status) {},
	}

	cc, err := controlclient.NewNoStart(opts)
	if err != nil {
		t.Fatal(err)
	}
	return cc
}

func fakeNoiseServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *http.Client) {
	ts := httptest.NewUnstartedServer(handler)
	ts.StartTLS()
	client := ts.Client()
	client.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true
	client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, ts.Listener.Addr().String())
	}
	return ts, client
}

func TestTKAEnablementFlow(t *testing.T) {
	networkLockAvailable = func() bool { return true } // Enable the feature flag
	nodePriv := key.NewNode()

	// Make a fake TKA authority, getting a usable genesis AUM which
	// our mock server can communicate.
	nlPriv := key.NewNLPrivate()
	key := tka.Key{Kind: tka.Key25519, Public: nlPriv.Public().Verifier(), Votes: 2}
	a1, genesisAUM, err := tka.Create(&tka.Mem{}, tka.State{
		Keys:               []tka.Key{key},
		DisablementSecrets: [][]byte{bytes.Repeat([]byte{0xa5}, 32)},
	}, nlPriv)
	if err != nil {
		t.Fatalf("tka.Create() failed: %v", err)
	}

	ts, client := fakeNoiseServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		switch r.URL.Path {
		case "/machine/tka/bootstrap":
			body := new(tailcfg.TKABootstrapRequest)
			if err := json.NewDecoder(r.Body).Decode(body); err != nil {
				t.Fatal(err)
			}
			if body.Version != tailcfg.CurrentCapabilityVersion {
				t.Errorf("bootstrap CapVer = %v, want %v", body.Version, tailcfg.CurrentCapabilityVersion)
			}
			if body.NodeKey != nodePriv.Public() {
				t.Errorf("bootstrap nodeKey=%v, want %v", body.NodeKey, nodePriv.Public())
			}
			if body.Head != "" {
				t.Errorf("bootstrap head=%s, want empty hash", body.Head)
			}

			w.WriteHeader(200)
			out := tailcfg.TKABootstrapResponse{
				GenesisAUM: genesisAUM.Serialize(),
			}
			if err := json.NewEncoder(w).Encode(out); err != nil {
				t.Fatal(err)
			}

		case "/machine/tka/sync/offer", "/machine/tka/sync/send":
			t.Error("node attempted to sync, but should have been up to date")

		default:
			t.Errorf("unhandled endpoint path: %v", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer ts.Close()
	temp := t.TempDir()

	cc := fakeControlClient(t, client)
	b := LocalBackend{
		varRoot: temp,
		cc:      cc,
		ccAuto:  cc,
		logf:    t.Logf,
		prefs: &ipn.Prefs{
			Persist: &persist.Persist{PrivateNodeKey: nodePriv},
		},
	}

	b.mu.Lock()
	err = b.tkaSyncIfNeededLocked(&netmap.NetworkMap{
		TKAEnabled: true,
		TKAHead:    a1.Head(),
	})
	b.mu.Unlock()
	if err != nil {
		t.Errorf("tkaSyncIfNeededLocked() failed: %v", err)
	}
	if b.tka == nil {
		t.Fatal("tka was not initialized")
	}
	if b.tka.authority.Head() != a1.Head() {
		t.Errorf("authority.Head() = %x, want %x", b.tka.authority.Head(), a1.Head())
	}
}

func TestTKADisablementFlow(t *testing.T) {
	networkLockAvailable = func() bool { return true } // Enable the feature flag
	temp := t.TempDir()
	os.Mkdir(filepath.Join(temp, "tka"), 0755)
	nodePriv := key.NewNode()

	// Make a fake TKA authority, to seed local state.
	disablementSecret := bytes.Repeat([]byte{0xa5}, 32)
	nlPriv := key.NewNLPrivate()
	key := tka.Key{Kind: tka.Key25519, Public: nlPriv.Public().Verifier(), Votes: 2}
	chonk, err := tka.ChonkDir(filepath.Join(temp, "tka"))
	if err != nil {
		t.Fatal(err)
	}
	authority, _, err := tka.Create(chonk, tka.State{
		Keys:               []tka.Key{key},
		DisablementSecrets: [][]byte{tka.DisablementKDF(disablementSecret)},
	}, nlPriv)
	if err != nil {
		t.Fatalf("tka.Create() failed: %v", err)
	}

	returnWrongSecret := false
	ts, client := fakeNoiseServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		switch r.URL.Path {
		case "/machine/tka/bootstrap":
			body := new(tailcfg.TKABootstrapRequest)
			if err := json.NewDecoder(r.Body).Decode(body); err != nil {
				t.Fatal(err)
			}
			if body.Version != tailcfg.CurrentCapabilityVersion {
				t.Errorf("bootstrap CapVer = %v, want %v", body.Version, tailcfg.CurrentCapabilityVersion)
			}
			if body.NodeKey != nodePriv.Public() {
				t.Errorf("nodeKey=%v, want %v", body.NodeKey, nodePriv.Public())
			}
			var head tka.AUMHash
			if err := head.UnmarshalText([]byte(body.Head)); err != nil {
				t.Fatalf("failed unmarshal of body.Head: %v", err)
			}
			if head != authority.Head() {
				t.Errorf("reported head = %x, want %x", head, authority.Head())
			}

			var disablement []byte
			if returnWrongSecret {
				disablement = bytes.Repeat([]byte{0x42}, 32) // wrong secret
			} else {
				disablement = disablementSecret
			}

			w.WriteHeader(200)
			out := tailcfg.TKABootstrapResponse{
				DisablementSecret: disablement,
			}
			if err := json.NewEncoder(w).Encode(out); err != nil {
				t.Fatal(err)
			}

		default:
			t.Errorf("unhandled endpoint path: %v", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer ts.Close()

	cc := fakeControlClient(t, client)
	b := LocalBackend{
		varRoot: temp,
		cc:      cc,
		ccAuto:  cc,
		logf:    t.Logf,
		tka: &tkaState{
			authority: authority,
			storage:   chonk,
		},
		prefs: &ipn.Prefs{
			Persist: &persist.Persist{PrivateNodeKey: nodePriv},
		},
	}

	// Test that the wrong disablement secret does not shut down the authority.
	returnWrongSecret = true
	b.mu.Lock()
	err = b.tkaSyncIfNeededLocked(&netmap.NetworkMap{
		TKAEnabled: false,
		TKAHead:    authority.Head(),
	})
	b.mu.Unlock()
	if err != nil {
		t.Errorf("tkaSyncIfNeededLocked() failed: %v", err)
	}
	if b.tka == nil {
		t.Error("TKA was disabled despite incorrect disablement secret")
	}

	// Test the correct disablement secret shuts down the authority.
	returnWrongSecret = false
	b.mu.Lock()
	err = b.tkaSyncIfNeededLocked(&netmap.NetworkMap{
		TKAEnabled: false,
		TKAHead:    authority.Head(),
	})
	b.mu.Unlock()
	if err != nil {
		t.Errorf("tkaSyncIfNeededLocked() failed: %v", err)
	}

	if b.tka != nil {
		t.Fatal("tka was not shut down")
	}
	if _, err := os.Stat(b.chonkPath()); err == nil || !os.IsNotExist(err) {
		t.Errorf("os.Stat(chonkDir) = %v, want ErrNotExist", err)
	}
}

func TestTKASync(t *testing.T) {
	networkLockAvailable = func() bool { return true } // Enable the feature flag

	someKeyPriv := key.NewNLPrivate()
	someKey := tka.Key{Kind: tka.Key25519, Public: someKeyPriv.Public().Verifier(), Votes: 1}

	type tkaSyncScenario struct {
		name string
		// controlAUMs is called (if non-nil) to get any AUMs which the tka state
		// on control should be seeded with.
		controlAUMs func(*testing.T, *tka.Authority, tka.Chonk, tka.Signer) []tka.AUM
		// controlAUMs is called (if non-nil) to get any AUMs which the tka state
		// on the node should be seeded with.
		nodeAUMs func(*testing.T, *tka.Authority, tka.Chonk, tka.Signer) []tka.AUM
	}

	tcs := []tkaSyncScenario{
		{name: "up to date"},
		{
			name: "control has an update",
			controlAUMs: func(t *testing.T, a *tka.Authority, storage tka.Chonk, signer tka.Signer) []tka.AUM {
				b := a.NewUpdater(signer)
				if err := b.RemoveKey(someKey.ID()); err != nil {
					t.Fatal(err)
				}
				aums, err := b.Finalize(storage)
				if err != nil {
					t.Fatal(err)
				}
				return aums
			},
		},
		{
			// AKA 'control data loss' scenario
			name: "node has an update",
			nodeAUMs: func(t *testing.T, a *tka.Authority, storage tka.Chonk, signer tka.Signer) []tka.AUM {
				b := a.NewUpdater(signer)
				if err := b.RemoveKey(someKey.ID()); err != nil {
					t.Fatal(err)
				}
				aums, err := b.Finalize(storage)
				if err != nil {
					t.Fatal(err)
				}
				return aums
			},
		},
		{
			// AKA 'control data loss + update in the meantime' scenario
			name: "node and control diverge",
			controlAUMs: func(t *testing.T, a *tka.Authority, storage tka.Chonk, signer tka.Signer) []tka.AUM {
				b := a.NewUpdater(signer)
				if err := b.SetKeyMeta(someKey.ID(), map[string]string{"ye": "swiggity"}); err != nil {
					t.Fatal(err)
				}
				aums, err := b.Finalize(storage)
				if err != nil {
					t.Fatal(err)
				}
				return aums
			},
			nodeAUMs: func(t *testing.T, a *tka.Authority, storage tka.Chonk, signer tka.Signer) []tka.AUM {
				b := a.NewUpdater(signer)
				if err := b.SetKeyMeta(someKey.ID(), map[string]string{"ye": "swooty"}); err != nil {
					t.Fatal(err)
				}
				aums, err := b.Finalize(storage)
				if err != nil {
					t.Fatal(err)
				}
				return aums
			},
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			temp := t.TempDir()
			os.Mkdir(filepath.Join(temp, "tka"), 0755)
			nodePriv := key.NewNode()
			nlPriv := key.NewNLPrivate()

			// Setup the tka authority on the control plane.
			key := tka.Key{Kind: tka.Key25519, Public: nlPriv.Public().Verifier(), Votes: 2}
			controlStorage := &tka.Mem{}
			controlAuthority, bootstrap, err := tka.Create(controlStorage, tka.State{
				Keys:               []tka.Key{key, someKey},
				DisablementSecrets: [][]byte{bytes.Repeat([]byte{0xa5}, 32)},
			}, nlPriv)
			if err != nil {
				t.Fatalf("tka.Create() failed: %v", err)
			}
			if tc.controlAUMs != nil {
				if err := controlAuthority.Inform(controlStorage, tc.controlAUMs(t, controlAuthority, controlStorage, nlPriv)); err != nil {
					t.Fatalf("controlAuthority.Inform() failed: %v", err)
				}
			}

			// Setup the TKA authority on the node.
			nodeStorage, err := tka.ChonkDir(filepath.Join(temp, "tka"))
			if err != nil {
				t.Fatal(err)
			}
			nodeAuthority, err := tka.Bootstrap(nodeStorage, bootstrap)
			if err != nil {
				t.Fatalf("tka.Bootstrap() failed: %v", err)
			}
			if tc.nodeAUMs != nil {
				if err := nodeAuthority.Inform(nodeStorage, tc.nodeAUMs(t, nodeAuthority, nodeStorage, nlPriv)); err != nil {
					t.Fatalf("nodeAuthority.Inform() failed: %v", err)
				}
			}

			// Make a mock control server.
			ts, client := fakeNoiseServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer r.Body.Close()
				switch r.URL.Path {
				case "/machine/tka/sync/offer":
					body := new(tailcfg.TKASyncOfferRequest)
					if err := json.NewDecoder(r.Body).Decode(body); err != nil {
						t.Fatal(err)
					}
					t.Logf("got sync offer:\n%+v", body)
					nodeOffer, err := toSyncOffer(body.Head, body.Ancestors)
					if err != nil {
						t.Fatal(err)
					}
					controlOffer, err := controlAuthority.SyncOffer(controlStorage)
					if err != nil {
						t.Fatal(err)
					}
					sendAUMs, err := controlAuthority.MissingAUMs(controlStorage, nodeOffer)
					if err != nil {
						t.Fatal(err)
					}

					head, ancestors, err := fromSyncOffer(controlOffer)
					if err != nil {
						t.Fatal(err)
					}
					resp := tailcfg.TKASyncOfferResponse{
						Head:        head,
						Ancestors:   ancestors,
						MissingAUMs: make([]tkatype.MarshaledAUM, len(sendAUMs)),
					}
					for i, a := range sendAUMs {
						resp.MissingAUMs[i] = a.Serialize()
					}

					t.Logf("responding to sync offer with:\n%+v", resp)
					w.WriteHeader(200)
					if err := json.NewEncoder(w).Encode(resp); err != nil {
						t.Fatal(err)
					}

				case "/machine/tka/sync/send":
					body := new(tailcfg.TKASyncSendRequest)
					if err := json.NewDecoder(r.Body).Decode(body); err != nil {
						t.Fatal(err)
					}
					t.Logf("got sync send:\n%+v", body)
					toApply := make([]tka.AUM, len(body.MissingAUMs))
					for i, a := range body.MissingAUMs {
						if err := toApply[i].Unserialize(a); err != nil {
							t.Fatalf("decoding missingAUM[%d]: %v", i, err)
						}
					}

					if len(toApply) > 0 {
						if err := controlAuthority.Inform(controlStorage, toApply); err != nil {
							t.Fatalf("control.Inform(%+v) failed: %v", toApply, err)
						}
					}
					head, err := controlAuthority.Head().MarshalText()
					if err != nil {
						t.Fatal(err)
					}

					w.WriteHeader(200)
					if err := json.NewEncoder(w).Encode(tailcfg.TKASyncSendResponse{Head: string(head)}); err != nil {
						t.Fatal(err)
					}

				default:
					t.Errorf("unhandled endpoint path: %v", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer ts.Close()

			// Setup the client.
			cc := fakeControlClient(t, client)
			b := LocalBackend{
				varRoot: temp,
				cc:      cc,
				ccAuto:  cc,
				logf:    t.Logf,
				tka: &tkaState{
					authority: nodeAuthority,
					storage:   nodeStorage,
				},
				prefs: &ipn.Prefs{
					Persist: &persist.Persist{PrivateNodeKey: nodePriv},
				},
			}

			// Finally, lets trigger a sync.
			b.mu.Lock()
			err = b.tkaSyncIfNeededLocked(&netmap.NetworkMap{
				TKAEnabled: true,
				TKAHead:    controlAuthority.Head(),
			})
			b.mu.Unlock()
			if err != nil {
				t.Errorf("tkaSyncIfNeededLocked() failed: %v", err)
			}

			// Check that at the end of this ordeal, the node and the control
			// plane are in sync.
			if nodeHead, controlHead := b.tka.authority.Head(), controlAuthority.Head(); nodeHead != controlHead {
				t.Errorf("node head = %v, want %v", nodeHead, controlHead)
			}
		})
	}
}
