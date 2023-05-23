package main_test

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func LongEnough() time.Duration {
	return time.Duration(rand.Int63n(16)) * time.Millisecond
}

func TestTraceRace(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close") // force one dial per redirect
		http.Redirect(w, r, "", http.StatusTemporaryRedirect)
	}))
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

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, address)
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for n := 0; n < 16; n++ {
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

					req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://test.example.com:%s", port), nil)
					if err != nil {
						panic(err)
					}
					_, _ = client.Do(req)
				}(ctx)
			}
		}()
	}
	wg.Wait()
}
