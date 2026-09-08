package dns

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// startServer listens on 127.0.0.1:0, serves srv on it in the background,
// and returns the address to query. The listener is closed on test cleanup.
func startServer(t *testing.T, srv *Server) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	go func() { _ = srv.Serve(conn) }()
	return conn.LocalAddr().String()
}

func TestResolvesInternalApp(t *testing.T) {
	want := net.ParseIP("10.200.0.7")
	srv := &Server{
		Resolve: func(app string) []net.IP {
			if app == "hello" {
				return []net.IP{want}
			}
			return nil
		},
	}
	addr := startServer(t, srv)

	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion("hello.internal.", dns.TypeA)

	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %v, want success", resp.Rcode)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("len(Answer) = %d, want 1", len(resp.Answer))
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer type = %T, want *dns.A", resp.Answer[0])
	}
	if !a.A.Equal(want) {
		t.Errorf("A = %v, want %v", a.A, want)
	}
}

func TestUnknownAppIsNXDOMAIN(t *testing.T) {
	srv := &Server{Resolve: func(app string) []net.IP { return nil }}
	addr := startServer(t, srv)

	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion("nope.internal.", dns.TypeA)

	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %v, want NXDOMAIN", resp.Rcode)
	}
}

func TestOtherZonesForwardUpstream(t *testing.T) {
	// Fake upstream: answers any question with a fixed A record, never
	// touching the real network.
	upstreamWant := net.ParseIP("93.184.216.34")
	upstreamConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	t.Cleanup(func() { upstreamConn.Close() })
	upstreamAddr := upstreamConn.LocalAddr().String()
	go dns.ActivateAndServe(nil, upstreamConn, dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetReply(r)
		reply.Answer = append(reply.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5},
			A:   upstreamWant,
		})
		_ = w.WriteMsg(reply)
	}))

	srv := &Server{
		Resolve:  func(app string) []net.IP { return nil },
		Upstream: upstreamAddr,
	}
	addr := startServer(t, srv)

	// Give the upstream listener a moment to be ready to serve.
	time.Sleep(50 * time.Millisecond)

	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)

	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("len(Answer) = %d, want 1", len(resp.Answer))
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer type = %T, want *dns.A", resp.Answer[0])
	}
	if !a.A.Equal(upstreamWant) {
		t.Errorf("A = %v, want %v (forwarded, not local)", a.A, upstreamWant)
	}
}

func TestMultipleQuestionsIsFormatError(t *testing.T) {
	srv := &Server{Resolve: func(app string) []net.IP { return nil }}
	addr := startServer(t, srv)

	c := new(dns.Client)
	m := new(dns.Msg)
	m.Question = []dns.Question{
		{Name: "a.internal.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
		{Name: "b.internal.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
	}

	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp.Rcode != dns.RcodeFormatError {
		t.Fatalf("rcode = %v, want FormatError", resp.Rcode)
	}
}
