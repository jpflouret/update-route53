package main

import (
	"context"
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
	"github.com/aws/aws-sdk-go-v2/config"
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

	dnsName      = ""                              // DNS_NAME environment variable
	dnsTTL       = uint64(300)                     // DNS_TTL environment variable
	hostedZoneId = ""                              // HOSTED_ZONE_ID environment variable
	checkIPURL   = "http://checkip.amazonaws.com/" // CHECK_IP environment variable
	sleepPeriod  = 5 * time.Minute                 // SLEEP_PERIOD environment variable

	httpClient = &http.Client{Timeout: 10 * time.Second}

	logger zerolog.Logger
)

func init() {
	prometheus.MustRegister(updateDuration)
	prometheus.MustRegister(updatesTotal)
}

func updateRoute53(ctx context.Context, svc *route53.Client) {

	logger := logger // local copy of logger

	// Fetch current IP address
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkIPURL, nil)
	if err != nil {
		logger.Err(err).Msg("unable to create request")
		return
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		logger.Err(err).Msg("unable to fetch current address")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Err(err).Msg("unable to read response body")
		return
	}

	// Validate IP address
	ipstr := strings.TrimSpace(string(body))
	ip := net.ParseIP(ipstr)
	if ip == nil {
		logger.Error().
			Str("address", ipstr).
			Msg("unable to parse address")
		return
	}

	logger = logger.With().Str("currentAddress", ipstr).Logger()

	// Fetch current value of record in AWS Route53
	currentRecordValue, currentRecordTTL, err := getCurrentRecordValue(ctx, svc)
	if err != nil {
		logger.Err(err).Msg("unable to get current record value")
		return
	}

	logger = logger.With().
		Str("currentRecordValue", currentRecordValue).
		Uint64("currentRecordTTL", currentRecordTTL).Logger()

	// Check if the current IP is different from the record value
	if currentRecordValue == ipstr &&
		currentRecordTTL == dnsTTL {
		logger.Info().Msg("address has not changed")
		return
	}

	// Update the record in AWS Route53
	input := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &types.ChangeBatch{
			Changes: []types.Change{
				{
					Action: types.ChangeActionUpsert,
					ResourceRecordSet: &types.ResourceRecordSet{
						Name:            aws.String(dnsName),
						Type:            types.RRTypeA,
						TTL:             aws.Int64(int64(dnsTTL)),
						ResourceRecords: []types.ResourceRecord{{Value: aws.String(ipstr)}},
					},
				},
			},
		},
		HostedZoneId: aws.String("/hostedzone/" + hostedZoneId),
	}
	changeOutput, err := svc.ChangeResourceRecordSets(ctx, input)
	if err != nil {
		logger.Err(err).Msg("unable to change record sets")
		return
	}

	updatesTotal.Inc()
	logger = logger.With().Str("change", *changeOutput.ChangeInfo.Id).Logger()
	logger.Info().Msg("change submitted")

	// Wait until the changes are INSYNC (up to 5 minutes)
	waiter := route53.NewResourceRecordSetsChangedWaiter(svc)
	waitInput := &route53.GetChangeInput{
		Id: changeOutput.ChangeInfo.Id,
	}
	if err := waiter.Wait(ctx, waitInput, 5*time.Minute); err != nil {
		if ctx.Err() != nil {
			logger.Warn().Msg("shutting down, stopping INSYNC poll")
		} else {
			logger.Err(err).Msg("failed waiting for change to propagate")
		}
		return
	}

	// Fetch current value of record to confirm the change
	updatedRecordValue, updatedRecordTTL, err := getCurrentRecordValue(ctx, svc)
	if err != nil {
		logger.Err(err).Msg("unable to get updated record value")
		return
	}

	logger.Info().
		Str("updatedRecordValue", updatedRecordValue).
		Uint64("updatedRecordTTL", updatedRecordTTL).
		Msg("change propagated")
}

func getCurrentRecordValue(ctx context.Context, svc *route53.Client) (string, uint64, error) {
	input := &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String("/hostedzone/" + hostedZoneId),
	}
	for {
		output, err := svc.ListResourceRecordSets(ctx, input)
		if err != nil {
			return "", 0, err
		}
		for _, recordSet := range output.ResourceRecordSets {
			if *recordSet.Name == (dnsName+".") && recordSet.Type == types.RRTypeA {
				return *recordSet.ResourceRecords[0].Value, uint64(*recordSet.TTL), nil
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

func main() {
	var err error

	console := flag.Bool("console", false, "enable console logging")
	port := flag.Uint("port", 8080, "port for health check/metrics server")
	flag.Parse()

	if *console {
		logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).With().Timestamp().Logger()
	} else {
		logger = zerolog.New(os.Stdout).With().Timestamp().Logger()
	}

	if *port < 1 || *port > 65535 {
		logger.Fatal().Msg("invalid port number")
	}

	dnsName = os.Getenv("DNS_NAME")
	if dnsName == "" {
		logger.Fatal().Msg("missing DNS_NAME environment variable")
	}

	dnsTTLStr := os.Getenv("DNS_TTL")
	if dnsTTLStr != "" {
		dnsTTL, err = strconv.ParseUint(dnsTTLStr, 10, 32)
		if err != nil {
			logger.Fatal().Msg("invalid DNS_TTL environment variable")
		}
	}

	hostedZoneId = os.Getenv("HOSTED_ZONE_ID")
	if hostedZoneId == "" {
		logger.Fatal().Msg("missing HOSTED_ZONE_ID environment variable")
	}

	tmpCheckIPURL := os.Getenv("CHECK_IP")
	if tmpCheckIPURL != "" {
		_, err := url.Parse(tmpCheckIPURL)
		if err != nil {
			logger.Fatal().Msg("invalid CHECK_IP environment variable")
		}
		checkIPURL = tmpCheckIPURL
	}

	sleepPeriodStr := os.Getenv("SLEEP_PERIOD")
	if sleepPeriodStr != "" {
		sleepPeriod, err = time.ParseDuration(sleepPeriodStr)
		if err != nil {
			logger.Fatal().Msg("invalid SLEEP_PERIOD environment variable")
		}
	}

	logger = logger.With().
		Str("dnsName", dnsName).
		Str("hostedZoneId", hostedZoneId).
		Logger()

	// Log startup message
	logger.Info().
		Str("checkIPURL", checkIPURL).
		Str("sleepPeriod", sleepPeriod.String()).
		Uint64("dnsTTL", dnsTTL).
		Msg("starting route53-updater...")

	// Cancel context on SIGINT/SIGTERM
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Load AWS configuration
	cfgCtx, cfgCancel := context.WithTimeout(ctx, 30*time.Second)
	defer cfgCancel()

	cfg, err := config.LoadDefaultConfig(cfgCtx)
	if err != nil {
		logger.Fatal().Err(err).Msg("unable to load aws configuration")
	}

	// Create Route53 client
	svc := route53.NewFromConfig(cfg)

	// Start health check server
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "200 OK")
	})
	mux.Handle("/metrics", promhttp.Handler())

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: mux,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal().Err(err).Msg("server failed")
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Err(err).Msg("server shutdown error")
		}
	}()

	// Start the main loop
	sleepTimer := time.NewTimer(0)
	defer sleepTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info().Msg("shutting down")
			return
		case <-sleepTimer.C:
		}

		// Start the duration timer
		start := time.Now()

		// Update Route53
		updateRoute53(ctx, svc)

		// Record the duration
		updateDuration.Add(float64(time.Since(start).Seconds()))

		// Wait before checking again
		sleepTimer.Reset(sleepPeriod)
	}
}
