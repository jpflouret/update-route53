package main

import (
	"context"
	"fmt"
	"io"
	"log"
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
	"github.com/go-logfmt/logfmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	updateDuration = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "update_route53_duration_total",
		Help: "Duration for updating Route53",
	})

	dnsName      = ""                              // DNS_NAME environment variable
	dnsTTL       = 300                             // DNS_TTL environment variable
	hostedZoneId = ""                              // HOSTED_ZONE_ID environment variable
	checkIPURL   = "http://checkip.amazonaws.com/" // CHECK_IP environment variable
	sleepPeriod  = 5 * time.Minute                 // SLEEP_PERIOD environment variable
)

func init() {
	prometheus.MustRegister(updateDuration)
}

func updateRoute53(svc *route53.Client) {
	enc := logfmt.NewEncoder(os.Stdout)

	// Fetch current IP address
	resp, err := http.Get(checkIPURL)
	if err != nil {
		enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
		enc.EncodeKeyval("level", "error")
		enc.EncodeKeyval("dnsName", dnsName)
		enc.EncodeKeyval("hostedZoneId", hostedZoneId)
		enc.EncodeKeyval("msg", fmt.Sprintf("unable to fetch address, %v", err))
		enc.EncodeKeyval("checkIPURL", checkIPURL)
		enc.EndRecord()
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
		enc.EncodeKeyval("level", "error")
		enc.EncodeKeyval("dnsName", dnsName)
		enc.EncodeKeyval("hostedZoneId", hostedZoneId)
		enc.EncodeKeyval("msg", fmt.Sprintf("unable to read response body, %v", err))
		enc.EncodeKeyval("checkIPURL", checkIPURL)
		enc.EndRecord()
		return
	}

	// Validate IP address
	ipstr := strings.TrimSpace(string(body))
	ip := net.ParseIP(ipstr)
	if ip == nil {
		enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
		enc.EncodeKeyval("level", "error")
		enc.EncodeKeyval("dnsName", dnsName)
		enc.EncodeKeyval("hostedZoneId", hostedZoneId)
		enc.EncodeKeyval("msg", fmt.Sprintf("unable to parse address, %v", ipstr))
		enc.EncodeKeyval("checkIPURL", checkIPURL)
		enc.EndRecord()
		return
	}

	// Fetch current value of record in AWS Route53
	currentRecordValue, err := getCurrentRecordValue(svc)
	if err != nil {
		enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
		enc.EncodeKeyval("level", "error")
		enc.EncodeKeyval("dnsName", dnsName)
		enc.EncodeKeyval("hostedZoneId", hostedZoneId)
		enc.EncodeKeyval("msg", fmt.Sprintf("unable to get current record value, %v", err))
		enc.EndRecord()
		return
	}

	// Check if the current IP is different from the record value
	if currentRecordValue == ipstr {
		enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
		enc.EncodeKeyval("level", "info")
		enc.EncodeKeyval("dnsName", dnsName)
		enc.EncodeKeyval("hostedZoneId", hostedZoneId)
		enc.EncodeKeyval("msg", "address has not changed")
		enc.EncodeKeyval("currentAddress", ipstr)
		enc.EncodeKeyval("currentRecordValue", currentRecordValue)
		enc.EndRecord()
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
		enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
		enc.EncodeKeyval("level", "error")
		enc.EncodeKeyval("dnsName", dnsName)
		enc.EncodeKeyval("hostedZoneId", hostedZoneId)
		enc.EncodeKeyval("msg", fmt.Sprintf("unable to change record sets, %v", err))
		enc.EndRecord()
		return
	}

	enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
	enc.EncodeKeyval("level", "info")
	enc.EncodeKeyval("dnsName", dnsName)
	enc.EncodeKeyval("hostedZoneId", hostedZoneId)
	enc.EncodeKeyval("msg", "change submitted")
	enc.EncodeKeyval("currentAddress", ipstr)
	enc.EncodeKeyval("currentRecordValue", currentRecordValue)
	enc.EncodeKeyval("change", *changeOutput.ChangeInfo.Id)
	enc.EncodeKeyval("dnsTTL", dnsTTL)
	enc.EndRecord()

	// Wait until the changes are INSYNC
	for {
		resp, err := svc.GetChange(context.TODO(), &route53.GetChangeInput{
			Id: aws.String(*changeOutput.ChangeInfo.Id),
		})
		if err != nil {
			enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
			enc.EncodeKeyval("level", "error")
			enc.EncodeKeyval("dnsName", dnsName)
			enc.EncodeKeyval("hostedZoneId", hostedZoneId)
			enc.EncodeKeyval("msg", fmt.Sprintf("unable to get change status, %v", err))
			enc.EncodeKeyval("currentAddress", ipstr)
			enc.EncodeKeyval("currentRecordValue", currentRecordValue)
			enc.EncodeKeyval("change", *changeOutput.ChangeInfo.Id)
			enc.EndRecord()
			break
		}

		if resp.ChangeInfo.Status == types.ChangeStatusInsync {

			// Fetch current value of record again to confirm the change
			currentRecordValue, err = getCurrentRecordValue(svc)
			if err != nil {
				enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
				enc.EncodeKeyval("level", "error")
				enc.EncodeKeyval("dnsName", dnsName)
				enc.EncodeKeyval("hostedZoneId", hostedZoneId)
				enc.EncodeKeyval("msg", fmt.Sprintf("unable to get current record value, %v", err))
				enc.EndRecord()
				return
			}

			enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
			enc.EncodeKeyval("level", "info")
			enc.EncodeKeyval("dnsName", dnsName)
			enc.EncodeKeyval("hostedZoneId", hostedZoneId)
			enc.EncodeKeyval("msg", "change propagated")
			enc.EncodeKeyval("currentAddress", ipstr)
			enc.EncodeKeyval("currentRecordValue", currentRecordValue)
			enc.EncodeKeyval("change", *changeOutput.ChangeInfo.Id)
			enc.EndRecord()
			break
		}

		// Wait 10 seconds before checking again
		time.Sleep(10 * time.Second)
	}
}

func getCurrentRecordValue(svc *route53.Client) (string, error) {
	listInput := &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String("/hostedzone/" + hostedZoneId),
	}
	listOutput, err := svc.ListResourceRecordSets(context.TODO(), listInput)
	if err != nil {
		return "", err
	}
	var currentRecordValue string
	for _, recordSet := range listOutput.ResourceRecordSets {
		if *recordSet.Name == (dnsName+".") && recordSet.Type == types.RRTypeA {
			currentRecordValue = *recordSet.ResourceRecords[0].Value
			break
		}
	}
	return currentRecordValue, nil
}

func main() {
	var err error
	enc := logfmt.NewEncoder(os.Stdout)

	dnsName = os.Getenv("DNS_NAME")
	if dnsName == "" {
		enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
		enc.EncodeKeyval("level", "error")
		enc.EncodeKeyval("msg", "missing DNS_NAME environment variable")
		enc.EndRecord()
		os.Exit(1)
	}

	dnsTTLStr := os.Getenv("DNS_TTL")
	if dnsTTLStr != "" {
		dnsTTL, err = strconv.Atoi(dnsTTLStr)
		if err != nil {
			enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
			enc.EncodeKeyval("level", "error")
			enc.EncodeKeyval("msg", "invalid DNS_TTL environment variable")
			enc.EndRecord()
			os.Exit(1)
		}
	}

	hostedZoneId = os.Getenv("HOSTED_ZONE_ID")
	if hostedZoneId == "" {
		enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
		enc.EncodeKeyval("level", "error")
		enc.EncodeKeyval("msg", "missing HOSTED_ZONE_ID environment variable")
		enc.EndRecord()
		os.Exit(1)
	}

	tmpCheckIPURL := os.Getenv("CHECK_IP")
	if tmpCheckIPURL != "" {
		_, err := url.Parse(tmpCheckIPURL)
		if err != nil {
			enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
			enc.EncodeKeyval("level", "error")
			enc.EncodeKeyval("msg", "invalid CHECK_IP environment variable")
			enc.EndRecord()
			os.Exit(1)
		}
		checkIPURL = tmpCheckIPURL
	}

	sleepPeriodStr := os.Getenv("SLEEP_PERIOD")
	if sleepPeriodStr != "" {
		sleepPeriod, err = time.ParseDuration(sleepPeriodStr)
		if err != nil {
			enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
			enc.EncodeKeyval("level", "error")
			enc.EncodeKeyval("msg", "invalid SLEEP_PERIOD environment variable")
			enc.EndRecord()
			os.Exit(1)
		}
	}

	accessKeyId := os.Getenv("AWS_ACCESS_KEY_ID")
	secretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	if accessKeyId == "" || secretAccessKey == "" {
		enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
		enc.EncodeKeyval("level", "error")
		enc.EncodeKeyval("msg", "missing AWS_ACCESS_KEY_ID or AWS_SECRET_ACCESS_KEY environment variable")
		enc.EndRecord()
		os.Exit(1)
	}
	region := os.Getenv("AWS_DEFAULT_REGION")
	if region == "" {
		region = "us-west-2"
		os.Setenv("AWS_DEFAULT_REGION", region)
	}

	// Log startup message
	enc.EncodeKeyval("time", time.Now().UTC().Format(time.RFC3339))
	enc.EncodeKeyval("level", "info")
	enc.EncodeKeyval("dnsName", dnsName)
	enc.EncodeKeyval("hostedZoneId", hostedZoneId)
	enc.EncodeKeyval("msg", "starting route53-updater...")
	enc.EncodeKeyval("dnsTTL", dnsTTL)
	enc.EncodeKeyval("checkIPURL", checkIPURL)
	enc.EncodeKeyval("sleepPeriod", sleepPeriod)
	enc.EndRecord()

	// Load AWS configuration from environment variables
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Fatalf("unable to load aws config from envirionment, %v", err)
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

		http.ListenAndServe(":8080", nil)
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
