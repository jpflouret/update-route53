package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/rs/zerolog"
)

// --- Mocks ---

type mockRoute53 struct {
	listFn   func(ctx context.Context, input *route53.ListResourceRecordSetsInput) (*route53.ListResourceRecordSetsOutput, error)
	changeFn func(ctx context.Context, input *route53.ChangeResourceRecordSetsInput) (*route53.ChangeResourceRecordSetsOutput, error)
}

func (m *mockRoute53) ListResourceRecordSets(ctx context.Context, input *route53.ListResourceRecordSetsInput, _ ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error) {
	return m.listFn(ctx, input)
}

func (m *mockRoute53) ChangeResourceRecordSets(ctx context.Context, input *route53.ChangeResourceRecordSetsInput, _ ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error) {
	return m.changeFn(ctx, input)
}

type mockWaiter struct {
	waitFn func(ctx context.Context, input *route53.GetChangeInput, dur time.Duration) error
}

func (m *mockWaiter) Wait(ctx context.Context, input *route53.GetChangeInput, dur time.Duration, _ ...func(*route53.ResourceRecordSetsChangedWaiterOptions)) error {
	if m.waitFn != nil {
		return m.waitFn(ctx, input, dur)
	}
	return nil
}

// --- Helpers ---

func newTestUpdater(r53 *mockRoute53, waiter *mockWaiter, ipServer *httptest.Server) *updater {
	return &updater{
		cfg: appConfig{
			dnsName:      "home.example.com",
			dnsTTL:       300,
			hostedZoneID: "ZXXXXXXXXX",
			checkIPURL:   ipServer.URL,
		},
		log:        zerolog.Nop(),
		r53:        r53,
		waiter:     waiter,
		httpClient: ipServer.Client(),
	}
}

func makeEnv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

// --- Tests ---

func TestParseConfig(t *testing.T) {
	baseEnv := map[string]string{
		"DNS_NAME":       "home.example.com",
		"HOSTED_ZONE_ID": "ZXXXXXXXXX",
	}

	t.Run("valid defaults", func(t *testing.T) {
		cfg, err := parseConfig(nil, makeEnv(baseEnv))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.dnsName != "home.example.com" {
			t.Errorf("dnsName = %q, want %q", cfg.dnsName, "home.example.com")
		}
		if cfg.hostedZoneID != "ZXXXXXXXXX" {
			t.Errorf("hostedZoneID = %q, want %q", cfg.hostedZoneID, "ZXXXXXXXXX")
		}
		if cfg.dnsTTL != 300 {
			t.Errorf("dnsTTL = %d, want 300", cfg.dnsTTL)
		}
		if cfg.port != 8080 {
			t.Errorf("port = %d, want 8080", cfg.port)
		}
		if cfg.sleepPeriod != 5*time.Minute {
			t.Errorf("sleepPeriod = %v, want 5m", cfg.sleepPeriod)
		}
		if cfg.checkIPURL != "http://checkip.amazonaws.com/" {
			t.Errorf("checkIPURL = %q, want default", cfg.checkIPURL)
		}
	})

	t.Run("custom values", func(t *testing.T) {
		env := map[string]string{
			"DNS_NAME":       "home.example.com",
			"HOSTED_ZONE_ID": "ZXXXXXXXXX",
			"DNS_TTL":        "60",
			"CHECK_IP":       "http://myip.example.com/",
			"SLEEP_PERIOD":   "10m",
		}
		cfg, err := parseConfig([]string{"-port", "9090", "-console"}, makeEnv(env))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.dnsTTL != 60 {
			t.Errorf("dnsTTL = %d, want 60", cfg.dnsTTL)
		}
		if cfg.port != 9090 {
			t.Errorf("port = %d, want 9090", cfg.port)
		}
		if !cfg.console {
			t.Error("console = false, want true")
		}
		if cfg.checkIPURL != "http://myip.example.com/" {
			t.Errorf("checkIPURL = %q, want custom", cfg.checkIPURL)
		}
		if cfg.sleepPeriod != 10*time.Minute {
			t.Errorf("sleepPeriod = %v, want 10m", cfg.sleepPeriod)
		}
	})

	t.Run("missing DNS_NAME", func(t *testing.T) {
		_, err := parseConfig(nil, makeEnv(map[string]string{"HOSTED_ZONE_ID": "Z123"}))
		if err == nil {
			t.Fatal("expected error for missing DNS_NAME")
		}
	})

	t.Run("missing HOSTED_ZONE_ID", func(t *testing.T) {
		_, err := parseConfig(nil, makeEnv(map[string]string{"DNS_NAME": "example.com"}))
		if err == nil {
			t.Fatal("expected error for missing HOSTED_ZONE_ID")
		}
	})

	t.Run("invalid DNS_TTL", func(t *testing.T) {
		env := map[string]string{
			"DNS_NAME":       "example.com",
			"HOSTED_ZONE_ID": "Z123",
			"DNS_TTL":        "abc",
		}
		_, err := parseConfig(nil, makeEnv(env))
		if err == nil {
			t.Fatal("expected error for invalid DNS_TTL")
		}
	})

	t.Run("invalid SLEEP_PERIOD", func(t *testing.T) {
		env := map[string]string{
			"DNS_NAME":       "example.com",
			"HOSTED_ZONE_ID": "Z123",
			"SLEEP_PERIOD":   "not-a-duration",
		}
		_, err := parseConfig(nil, makeEnv(env))
		if err == nil {
			t.Fatal("expected error for invalid SLEEP_PERIOD")
		}
	})

	t.Run("invalid port zero", func(t *testing.T) {
		_, err := parseConfig([]string{"-port", "0"}, makeEnv(baseEnv))
		if err == nil {
			t.Fatal("expected error for port 0")
		}
	})
}

func TestCurrentRecord(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		mock := &mockRoute53{
			listFn: func(_ context.Context, _ *route53.ListResourceRecordSetsInput) (*route53.ListResourceRecordSetsOutput, error) {
				return &route53.ListResourceRecordSetsOutput{
					ResourceRecordSets: []types.ResourceRecordSet{
						{
							Name:            aws.String("home.example.com."),
							Type:            types.RRTypeA,
							TTL:             aws.Int64(300),
							ResourceRecords: []types.ResourceRecord{{Value: aws.String("1.2.3.4")}},
						},
					},
				}, nil
			},
		}
		u := &updater{
			cfg: appConfig{dnsName: "home.example.com", hostedZoneID: "Z123"},
			r53: mock,
		}
		value, ttl, err := u.currentRecord(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if value != "1.2.3.4" {
			t.Errorf("value = %q, want %q", value, "1.2.3.4")
		}
		if ttl != 300 {
			t.Errorf("ttl = %d, want 300", ttl)
		}
	})

	t.Run("not found", func(t *testing.T) {
		mock := &mockRoute53{
			listFn: func(_ context.Context, _ *route53.ListResourceRecordSetsInput) (*route53.ListResourceRecordSetsOutput, error) {
				return &route53.ListResourceRecordSetsOutput{}, nil
			},
		}
		u := &updater{
			cfg: appConfig{dnsName: "home.example.com", hostedZoneID: "Z123"},
			r53: mock,
		}
		value, ttl, err := u.currentRecord(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if value != "" || ttl != 0 {
			t.Errorf("expected empty result, got value=%q ttl=%d", value, ttl)
		}
	})

	t.Run("API error", func(t *testing.T) {
		mock := &mockRoute53{
			listFn: func(_ context.Context, _ *route53.ListResourceRecordSetsInput) (*route53.ListResourceRecordSetsOutput, error) {
				return nil, fmt.Errorf("access denied")
			},
		}
		u := &updater{
			cfg: appConfig{dnsName: "home.example.com", hostedZoneID: "Z123"},
			r53: mock,
		}
		_, _, err := u.currentRecord(context.Background())
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("pagination", func(t *testing.T) {
		call := 0
		mock := &mockRoute53{
			listFn: func(_ context.Context, _ *route53.ListResourceRecordSetsInput) (*route53.ListResourceRecordSetsOutput, error) {
				call++
				if call == 1 {
					return &route53.ListResourceRecordSetsOutput{
						IsTruncated:    true,
						NextRecordName: aws.String("other.example.com."),
						NextRecordType: types.RRTypeA,
					}, nil
				}
				return &route53.ListResourceRecordSetsOutput{
					ResourceRecordSets: []types.ResourceRecordSet{
						{
							Name:            aws.String("home.example.com."),
							Type:            types.RRTypeA,
							TTL:             aws.Int64(60),
							ResourceRecords: []types.ResourceRecord{{Value: aws.String("5.6.7.8")}},
						},
					},
				}, nil
			},
		}
		u := &updater{
			cfg: appConfig{dnsName: "home.example.com", hostedZoneID: "Z123"},
			r53: mock,
		}
		value, ttl, err := u.currentRecord(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if value != "5.6.7.8" || ttl != 60 {
			t.Errorf("got value=%q ttl=%d, want 5.6.7.8/60", value, ttl)
		}
		if call != 2 {
			t.Errorf("expected 2 API calls, got %d", call)
		}
	})
}

func TestUpdate(t *testing.T) {
	t.Run("no change needed", func(t *testing.T) {
		ipServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "1.2.3.4\n")
		}))
		defer ipServer.Close()

		changeCalled := false
		mock := &mockRoute53{
			listFn: func(_ context.Context, _ *route53.ListResourceRecordSetsInput) (*route53.ListResourceRecordSetsOutput, error) {
				return &route53.ListResourceRecordSetsOutput{
					ResourceRecordSets: []types.ResourceRecordSet{
						{
							Name:            aws.String("home.example.com."),
							Type:            types.RRTypeA,
							TTL:             aws.Int64(300),
							ResourceRecords: []types.ResourceRecord{{Value: aws.String("1.2.3.4")}},
						},
					},
				}, nil
			},
			changeFn: func(_ context.Context, _ *route53.ChangeResourceRecordSetsInput) (*route53.ChangeResourceRecordSetsOutput, error) {
				changeCalled = true
				return nil, nil
			},
		}

		u := newTestUpdater(mock, &mockWaiter{}, ipServer)
		if err := u.update(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if changeCalled {
			t.Error("ChangeResourceRecordSets should not have been called")
		}
	})

	t.Run("IP changed", func(t *testing.T) {
		ipServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "5.6.7.8\n")
		}))
		defer ipServer.Close()

		var gotInput *route53.ChangeResourceRecordSetsInput
		mock := &mockRoute53{
			listFn: func(_ context.Context, _ *route53.ListResourceRecordSetsInput) (*route53.ListResourceRecordSetsOutput, error) {
				return &route53.ListResourceRecordSetsOutput{
					ResourceRecordSets: []types.ResourceRecordSet{
						{
							Name:            aws.String("home.example.com."),
							Type:            types.RRTypeA,
							TTL:             aws.Int64(300),
							ResourceRecords: []types.ResourceRecord{{Value: aws.String("1.2.3.4")}},
						},
					},
				}, nil
			},
			changeFn: func(_ context.Context, input *route53.ChangeResourceRecordSetsInput) (*route53.ChangeResourceRecordSetsOutput, error) {
				gotInput = input
				return &route53.ChangeResourceRecordSetsOutput{
					ChangeInfo: &types.ChangeInfo{
						Id: aws.String("/change/C123"),
					},
				}, nil
			},
		}

		u := newTestUpdater(mock, &mockWaiter{}, ipServer)
		if err := u.update(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotInput == nil {
			t.Fatal("ChangeResourceRecordSets was not called")
		}
		if got := *gotInput.HostedZoneId; got != "/hostedzone/ZXXXXXXXXX" {
			t.Errorf("HostedZoneId = %q, want %q", got, "/hostedzone/ZXXXXXXXXX")
		}
		change := gotInput.ChangeBatch.Changes[0]
		if change.Action != types.ChangeActionUpsert {
			t.Errorf("Action = %q, want %q", change.Action, types.ChangeActionUpsert)
		}
		rs := change.ResourceRecordSet
		if got := *rs.Name; got != "home.example.com" {
			t.Errorf("Name = %q, want %q", got, "home.example.com")
		}
		if rs.Type != types.RRTypeA {
			t.Errorf("Type = %q, want %q", rs.Type, types.RRTypeA)
		}
		if got := *rs.TTL; got != 300 {
			t.Errorf("TTL = %d, want 300", got)
		}
		if got := *rs.ResourceRecords[0].Value; got != "5.6.7.8" {
			t.Errorf("Value = %q, want %q", got, "5.6.7.8")
		}
	})

	t.Run("TTL changed", func(t *testing.T) {
		ipServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "1.2.3.4\n")
		}))
		defer ipServer.Close()

		var gotInput *route53.ChangeResourceRecordSetsInput
		mock := &mockRoute53{
			listFn: func(_ context.Context, _ *route53.ListResourceRecordSetsInput) (*route53.ListResourceRecordSetsOutput, error) {
				return &route53.ListResourceRecordSetsOutput{
					ResourceRecordSets: []types.ResourceRecordSet{
						{
							Name:            aws.String("home.example.com."),
							Type:            types.RRTypeA,
							TTL:             aws.Int64(60), // different TTL
							ResourceRecords: []types.ResourceRecord{{Value: aws.String("1.2.3.4")}},
						},
					},
				}, nil
			},
			changeFn: func(_ context.Context, input *route53.ChangeResourceRecordSetsInput) (*route53.ChangeResourceRecordSetsOutput, error) {
				gotInput = input
				return &route53.ChangeResourceRecordSetsOutput{
					ChangeInfo: &types.ChangeInfo{
						Id: aws.String("/change/C456"),
					},
				}, nil
			},
		}

		u := newTestUpdater(mock, &mockWaiter{}, ipServer)
		if err := u.update(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotInput == nil {
			t.Fatal("ChangeResourceRecordSets was not called")
		}
		if got := *gotInput.HostedZoneId; got != "/hostedzone/ZXXXXXXXXX" {
			t.Errorf("HostedZoneId = %q, want %q", got, "/hostedzone/ZXXXXXXXXX")
		}
		change := gotInput.ChangeBatch.Changes[0]
		if change.Action != types.ChangeActionUpsert {
			t.Errorf("Action = %q, want %q", change.Action, types.ChangeActionUpsert)
		}
		rs := change.ResourceRecordSet
		if got := *rs.Name; got != "home.example.com" {
			t.Errorf("Name = %q, want %q", got, "home.example.com")
		}
		if rs.Type != types.RRTypeA {
			t.Errorf("Type = %q, want %q", rs.Type, types.RRTypeA)
		}
		if got := *rs.TTL; got != 300 {
			t.Errorf("TTL = %d, want 300", got)
		}
		if got := *rs.ResourceRecords[0].Value; got != "1.2.3.4" {
			t.Errorf("Value = %q, want %q", got, "1.2.3.4")
		}
	})

	t.Run("invalid IP response", func(t *testing.T) {
		ipServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "not-an-ip")
		}))
		defer ipServer.Close()

		u := newTestUpdater(&mockRoute53{}, &mockWaiter{}, ipServer)
		err := u.update(context.Background())
		if err == nil {
			t.Fatal("expected error for invalid IP")
		}
	})

	t.Run("Route53 list error", func(t *testing.T) {
		ipServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "1.2.3.4\n")
		}))
		defer ipServer.Close()

		mock := &mockRoute53{
			listFn: func(_ context.Context, _ *route53.ListResourceRecordSetsInput) (*route53.ListResourceRecordSetsOutput, error) {
				return nil, fmt.Errorf("throttled")
			},
		}

		u := newTestUpdater(mock, &mockWaiter{}, ipServer)
		err := u.update(context.Background())
		if err == nil {
			t.Fatal("expected error for Route53 list failure")
		}
	})
}

func TestHealthz(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "200 OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Header().Get("Content-Type") != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", rec.Header().Get("Content-Type"))
	}
	if rec.Body.String() != "200 OK" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "200 OK")
	}
}
