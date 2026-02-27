package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
)

var (
	updateDuration = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "update_route53_duration_total",
		Help: "Duration for updating Route53",
	})

	updatesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "update_route53_updates_total",
		Help: "Total number of Route53 record updates performed",
	})
)

func init() {
	prometheus.MustRegister(updateDuration)
	prometheus.MustRegister(updatesTotal)
}

type appConfig struct {
	dnsName      string
	dnsTTL       int64
	hostedZoneID string
	checkIPURL   string
	sleepPeriod  time.Duration
	port         uint
	console      bool
}

type route53API interface {
	ChangeResourceRecordSets(ctx context.Context, params *route53.ChangeResourceRecordSetsInput, optFns ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error)
	ListResourceRecordSets(ctx context.Context, params *route53.ListResourceRecordSetsInput, optFns ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error)
}

type changeWaiter interface {
	Wait(ctx context.Context, params *route53.GetChangeInput, maxWaitDur time.Duration, optFns ...func(*route53.ResourceRecordSetsChangedWaiterOptions)) error
}

type updater struct {
	cfg        appConfig
	log        zerolog.Logger
	r53        route53API
	waiter     changeWaiter
	httpClient *http.Client
}

func parseConfig(args []string, getenv func(string) string) (appConfig, error) {
	fs := flag.NewFlagSet("update-route53", flag.ContinueOnError)
	console := fs.Bool("console", false, "enable console logging")
	port := fs.Uint("port", 8080, "port for health check/metrics server")
	if err := fs.Parse(args); err != nil {
		return appConfig{}, err
	}

	cfg := appConfig{
		console:     *console,
		port:        *port,
		checkIPURL:  "http://checkip.amazonaws.com/",
		dnsTTL:      300,
		sleepPeriod: 5 * time.Minute,
	}

	if cfg.port == 0 || cfg.port > 65535 {
		return appConfig{}, fmt.Errorf("invalid port number: %d", cfg.port)
	}

	cfg.dnsName = getenv("DNS_NAME")
	if cfg.dnsName == "" {
		return appConfig{}, fmt.Errorf("missing DNS_NAME environment variable")
	}

	cfg.hostedZoneID = getenv("HOSTED_ZONE_ID")
	if cfg.hostedZoneID == "" {
		return appConfig{}, fmt.Errorf("missing HOSTED_ZONE_ID environment variable")
	}

	if v := getenv("DNS_TTL"); v != "" {
		ttl, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return appConfig{}, fmt.Errorf("invalid DNS_TTL: %w", err)
		}
		cfg.dnsTTL = ttl
	}

	if v := getenv("CHECK_IP"); v != "" {
		if _, err := url.Parse(v); err != nil {
			return appConfig{}, fmt.Errorf("invalid CHECK_IP: %w", err)
		}
		cfg.checkIPURL = v
	}

	if v := getenv("SLEEP_PERIOD"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return appConfig{}, fmt.Errorf("invalid SLEEP_PERIOD: %w", err)
		}
		cfg.sleepPeriod = d
	}

	return cfg, nil
}

func (u *updater) currentRecord(ctx context.Context) (string, int64, error) {
	input := &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String("/hostedzone/" + u.cfg.hostedZoneID),
	}
	for {
		output, err := u.r53.ListResourceRecordSets(ctx, input)
		if err != nil {
			return "", 0, fmt.Errorf("listing record sets: %w", err)
		}
		for _, rs := range output.ResourceRecordSets {
			if *rs.Name == u.cfg.dnsName+"." && rs.Type == types.RRTypeA {
				return *rs.ResourceRecords[0].Value, *rs.TTL, nil
			}
		}
		if !output.IsTruncated {
			break
		}
		input.StartRecordName = output.NextRecordName
		input.StartRecordType = output.NextRecordType
		input.StartRecordIdentifier = output.NextRecordIdentifier
	}
	return "", 0, nil
}

func (u *updater) update(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.cfg.checkIPURL, nil)
	if err != nil {
		return fmt.Errorf("creating IP check request: %w", err)
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetching current address: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}

	rawIP := strings.TrimSpace(string(body))
	if net.ParseIP(rawIP) == nil {
		return fmt.Errorf("invalid IP address: %q", rawIP)
	}

	currentValue, currentTTL, err := u.currentRecord(ctx)
	if err != nil {
		return fmt.Errorf("getting current record: %w", err)
	}

	if currentValue == rawIP && currentTTL == u.cfg.dnsTTL {
		u.log.Info().
			Str("currentAddress", rawIP).
			Str("currentRecordValue", currentValue).
			Int64("currentRecordTTL", currentTTL).
			Msg("address has not changed")
		return nil
	}

	changeInput := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &types.ChangeBatch{
			Changes: []types.Change{
				{
					Action: types.ChangeActionUpsert,
					ResourceRecordSet: &types.ResourceRecordSet{
						Name:            aws.String(u.cfg.dnsName),
						Type:            types.RRTypeA,
						TTL:             aws.Int64(u.cfg.dnsTTL),
						ResourceRecords: []types.ResourceRecord{{Value: aws.String(rawIP)}},
					},
				},
			},
		},
		HostedZoneId: aws.String("/hostedzone/" + u.cfg.hostedZoneID),
	}
	changeOutput, err := u.r53.ChangeResourceRecordSets(ctx, changeInput)
	if err != nil {
		return fmt.Errorf("changing record sets: %w", err)
	}

	updatesTotal.Inc()
	changeID := *changeOutput.ChangeInfo.Id
	u.log.Info().
		Str("currentAddress", rawIP).
		Str("change", changeID).
		Msg("change submitted")

	waitInput := &route53.GetChangeInput{Id: changeOutput.ChangeInfo.Id}
	if err := u.waiter.Wait(ctx, waitInput, 5*time.Minute); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("shutting down during INSYNC poll: %w", ctx.Err())
		}
		return fmt.Errorf("waiting for change to propagate: %w", err)
	}

	updatedValue, updatedTTL, err := u.currentRecord(ctx)
	if err != nil {
		return fmt.Errorf("confirming updated record: %w", err)
	}

	u.log.Info().
		Str("currentAddress", rawIP).
		Str("change", changeID).
		Str("updatedRecordValue", updatedValue).
		Int64("updatedRecordTTL", updatedTTL).
		Msg("change propagated")

	return nil
}

func run(ctx context.Context, args []string, getenv func(string) string) error {
	cfg, err := parseConfig(args, getenv)
	if err != nil {
		return err
	}

	var logger zerolog.Logger
	if cfg.console {
		logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).With().Timestamp().Logger()
	} else {
		logger = zerolog.New(os.Stdout).With().Timestamp().Logger()
	}
	logger = logger.With().
		Str("dnsName", cfg.dnsName).
		Str("hostedZoneID", cfg.hostedZoneID).
		Logger()

	logger.Info().
		Str("checkIPURL", cfg.checkIPURL).
		Str("sleepPeriod", cfg.sleepPeriod.String()).
		Int64("dnsTTL", cfg.dnsTTL).
		Msg("starting route53-updater...")

	cfgCtx, cfgCancel := context.WithTimeout(ctx, 30*time.Second)
	defer cfgCancel()

	awsCfg, err := awsconfig.LoadDefaultConfig(cfgCtx)
	if err != nil {
		return fmt.Errorf("loading AWS configuration: %w", err)
	}

	svc := route53.NewFromConfig(awsCfg)

	u := &updater{
		cfg:        cfg,
		log:        logger,
		r53:        svc,
		waiter:     route53.NewResourceRecordSetsChangedWaiter(svc),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "200 OK")
	})
	mux.Handle("/metrics", promhttp.Handler())

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.port),
		Handler: mux,
	}

	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Err(err).Msg("server shutdown error")
		}
	}()

	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	sleepTimer := time.NewTimer(0)
	defer sleepTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info().Msg("shutting down")
			return nil
		case err := <-serverErr:
			return fmt.Errorf("server failed: %w", err)
		case <-sleepTimer.C:
		}

		start := time.Now()
		if err := u.update(ctx); err != nil {
			logger.Err(err).Msg("update failed")
		}
		updateDuration.Add(time.Since(start).Seconds())

		sleepTimer.Reset(cfg.sleepPeriod)
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
}
