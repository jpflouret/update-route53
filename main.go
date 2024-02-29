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
	"strconv"
	"strings"
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

	dnsName      = ""                              // DNS_NAME environment variable
	dnsTTL       = uint64(300)                     // DNS_TTL environment variable
	hostedZoneId = ""                              // HOSTED_ZONE_ID environment variable
	checkIPURL   = "http://checkip.amazonaws.com/" // CHECK_IP environment variable
	sleepPeriod  = 5 * time.Minute                 // SLEEP_PERIOD environment variable

	logger zerolog.Logger
)

func init() {
	prometheus.MustRegister(updateDuration)
}

func updateRoute53(svc *route53.Client) {

	logger := logger // local copy of logger

	// Fetch current IP address
	resp, err := http.Get(checkIPURL)
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
	currentRecordValue, currentRecordTTL, err := getCurrentRecordValue(svc)
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
	changeOutput, err := svc.ChangeResourceRecordSets(context.TODO(), input)
	if err != nil {
		logger.Err(err).Msg("unable to change record sets")
		return
	}

	logger = logger.With().Str("change", *changeOutput.ChangeInfo.Id).Logger()
	logger.Info().Msg("change submitted")

	// Wait until the changes are INSYNC
	for {
		resp, err := svc.GetChange(context.TODO(), &route53.GetChangeInput{
			Id: aws.String(*changeOutput.ChangeInfo.Id),
		})
		if err != nil {
			logger.Err(err).Msg("unable to get change status")
			break
		}

		if resp.ChangeInfo.Status == types.ChangeStatusInsync {

			// Fetch current value of record again to confirm the change
			updatedRecordValue, updatedRecordTTL, err := getCurrentRecordValue(svc)
			if err != nil {
				logger.Err(err).Msg("unable to get updated record value")
				return
			}

			logger.Info().
				Str("updatedRecordValue", updatedRecordValue).
				Uint64("updatedRecordTTL", updatedRecordTTL).
				Msg("change propagated")
			break
		}

		// Wait 10 seconds before checking again
		time.Sleep(10 * time.Second)
	}
}

func getCurrentRecordValue(svc *route53.Client) (string, uint64, error) {
	listInput := &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String("/hostedzone/" + hostedZoneId),
	}
	listOutput, err := svc.ListResourceRecordSets(context.TODO(), listInput)
	if err != nil {
		return "", 0, err
	}
	var currentRecordValue string
	var currentRecordTTL uint64
	for _, recordSet := range listOutput.ResourceRecordSets {
		if *recordSet.Name == (dnsName+".") && recordSet.Type == types.RRTypeA {
			currentRecordValue = *recordSet.ResourceRecords[0].Value
			currentRecordTTL = uint64(*recordSet.TTL)
			break
		}
	}
	return currentRecordValue, currentRecordTTL, nil
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

	// Load AWS configuration
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		logger.Err(err).Msg("unable to load aws configuration")
	}

	// Create Route53 client
	svc := route53.NewFromConfig(cfg)

	// Start health check server
	go func() {
		http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "200 OK")
		})

		// Add Prometheus metrics endpoint
		http.Handle("/metrics", promhttp.Handler())

		http.ListenAndServe(fmt.Sprintf(":%d", *port), nil)
	}()

	// Start the main loop
	for {
		// Start the duration timer
		start := time.Now()

		// Update Route53
		updateRoute53(svc)

		// Record the duration
		updateDuration.Add(float64(time.Since(start).Seconds()))

		// Wait before checking again
		time.Sleep(sleepPeriod)
	}
}
