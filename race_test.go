//go:build race

package main_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func LongEnough() time.Duration {
	return time.Duration(rand.Int63n(8)) * time.Millisecond
}

func TestTraceRace(t *testing.T) {
	var cumulativeRaceCount int

	for _, tc := range []struct {
		name  string
		http2 bool
	}{
		{
			name: "http1",
		},
		{
			name:  "http2",
			http2: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Connection", "close") // force one dial per redirect
				http.Redirect(w, r, "", http.StatusTemporaryRedirect)
			}))
			s.Config.ErrorLog = log.New(io.Discard, "", 0) // mute spam from cancelled tls handshake eofs
			if tc.http2 {
				s.EnableHTTP2 = true
				s.StartTLS()
			} else {
				s.Start()
			}
			defer s.Close()

			u, err := url.Parse(s.URL)
			if err != nil {
				t.Fatal(err)
			}

			host, port, err := net.SplitHostPort(u.Host)
			if err != nil {
				t.Fatal(err)
			}

			l, err := net.Listen("tcp", "localhost:0")
			if err != nil {
				t.Fatal(err)
			}
			ds := &dns.Server{
				Listener: l,
				Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
					time.Sleep(LongEnough())
					var m dns.Msg
					m.SetReply(r)
					m.Authoritative = true
					switch r.Question[0].Qtype {
					case dns.TypeAAAA:
						// resolver will try both ipv6 and ipv4 by default
						// assuming the test server is bound to an ipv4 address
						m.Rcode = dns.RcodeNameError
					case dns.TypeA:
						rr, err := dns.NewRR(fmt.Sprintf("%s\tIN\tA\t%s", r.Question[0].Name, host))
						if err != nil {
							panic(err)
						}
						m.Answer = append(m.Answer, rr)
					}
					w.WriteMsg(&m)
				}),
			}
			go ds.ActivateAndServe()

			dialer := &net.Dialer{
				Resolver: &net.Resolver{
					PreferGo: true,
					Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
						// send queries to the test dns server
						return net.Dial(l.Addr().Network(), l.Addr().String())
					},
				},
			}

			var tcc *tls.Config
			if tc.http2 {
				certpool := x509.NewCertPool()
				certpool.AddCert(s.Certificate())
				tcc = &tls.Config{
					NextProtos: []string{"h2"},
					RootCAs:    certpool,
				}
			}

			client := &http.Client{
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
						return dialer.DialContext(ctx, network, address)
					},
					TLSClientConfig: tcc,
				},
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s://example.com:%s", u.Scheme, port), nil)
			if err != nil {
				t.Fatal(err)
			}

			var wg sync.WaitGroup
			for n := 0; n < 128; n++ {
				wg.Add(1)
				go func() {
					defer wg.Done()

					for {
						select {
						case <-ctx.Done():
							return
						default:
						}

						func(ctx context.Context) {
							ctx, cancel := context.WithTimeout(ctx, LongEnough())
							defer cancel()

							var i int
							ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
								DNSStart: func(httptrace.DNSStartInfo) {
									i = i + 1
								},
								DNSDone: func(httptrace.DNSDoneInfo) {
									i = i - 1
								},
							})

							_, _ = client.Do(req.WithContext(ctx))
						}(ctx)
					}
				}()
			}
			wg.Wait()
		})

		if errs := runtime.RaceErrors() - cumulativeRaceCount; errs > 0 {
			t.Errorf("detected %d data races", errs)
			cumulativeRaceCount += errs
		}
	}
}
