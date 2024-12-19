package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RedHatInsights/sources-api-go/kafka"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redhatinsights/sources-superkey-worker/config"
	l "github.com/redhatinsights/sources-superkey-worker/logger"
	"github.com/redhatinsights/sources-superkey-worker/provider"
	"github.com/redhatinsights/sources-superkey-worker/superkey"
	"github.com/sirupsen/logrus"
)

const (
	superkeyRequestedTopic = "platform.sources.superkey-requests"
)

var (
	// DisableCreation disabled processing `create_application` sk requests
	DisableCreation = os.Getenv("DISABLE_RESOURCE_CREATION")
	// DisableDeletion disabled processing `destroy_application` sk requests
	DisableDeletion = os.Getenv("DISABLE_RESOURCE_DELETION")

	conf          = config.Get()
	superkeyTopic = conf.KafkaTopic(superkeyRequestedTopic)
)

func main() {
	l.InitLogger(conf)

	initMetrics()
	initHealthCheck()

	var brokers strings.Builder
	for i, broker := range conf.KafkaBrokerConfig {
		brokers.WriteString(broker.Hostname + ":" + strconv.Itoa(*broker.Port))
		if i < len(conf.KafkaBrokerConfig)-1 {
			brokers.WriteString(",")
		}
	}

	l.Log.Infof("Listening to Kafka at: %s, topic: %v", brokers.String(), superkeyTopic)
	l.Log.Infof("Talking to Sources API at: [%v] using PSK [%v]", fmt.Sprintf("%v://%v:%v", conf.SourcesScheme, conf.SourcesHost, conf.SourcesPort), conf.SourcesPSK)

	reader, err := kafka.GetReader(&kafka.Options{
		BrokerConfig: conf.KafkaBrokerConfig,
		Topic:        superkeyTopic,
		GroupID:      &conf.KafkaGroupID,
		Logger:       l.Log.WithField("kafka", ""),
	})
	if err != nil {
		l.Log.Fatalf(`could not get Kafka reader: %s`, err)
	}

	go func() {
		l.Log.Info("SuperKey Worker started.")

		kafka.Consume(
			reader,
			func(msg kafka.Message) {
				l.Log.Infof("Started processing message %s", string(msg.Value))
				processSuperkeyRequest(msg)
				l.Log.Infof("Finished processing message %s", string(msg.Value))
			},
		)
	}()

	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)

	// wait for a signal from the OS, gracefully terminating the consumer
	// if/when that comes in
	s := <-interrupts

	l.Log.Infof("Received %v, exiting", s)
	kafka.CloseReader(reader, "superkey reader")
	os.Exit(0)
}

// processSuperkeyRequest - processes messages.
func processSuperkeyRequest(msg kafka.Message) {
	eventType := msg.GetHeader("event_type")
	identityHeader := msg.GetHeader("x-rh-identity")
	orgIdHeader := msg.GetHeader("x-rh-sources-org-id")

	if identityHeader == "" && orgIdHeader == "" {
		l.Log.WithFields(logrus.Fields{"kafka_message": msg}).Error(`Skipping Superkey request because no "x-rh-identity" or "x-rh-sources-org-id" headers were found`)

		return
	}

	l.Log.WithFields(logrus.Fields{"org_id": orgIdHeader}).Debugf(`Processing Kafka message: %s`, string(msg.Value))

	switch eventType {
	case "create_application":
		req := &superkey.CreateRequest{}
		err := msg.ParseTo(req)
		if err != nil {
			l.Log.WithFields(logrus.Fields{"org_id": orgIdHeader}).Errorf(`Error parsing "create_application" request "%s": %v`, string(msg.Value), err)
			return
		}
		req.IdentityHeader = identityHeader
		req.OrgIdHeader = orgIdHeader

		if DisableCreation == "true" {
			l.Log.WithFields(logrus.Fields{"tenant_id": req.TenantID, "source_id": req.SourceID, "application_id": req.ApplicationID, "application_type": req.ApplicationType}).Infof("Skipping create_application request: %v", msg.Value)
			return
		}

		l.Log.WithFields(logrus.Fields{"tenant_id": req.TenantID, "source_id": req.SourceID, "application_id": req.ApplicationID, "application_type": req.ApplicationType}).Info(`Processing "create_application" request`)

		createResources(req)

		l.Log.WithFields(logrus.Fields{"tenant_id": req.TenantID, "source_id": req.SourceID, "application_id": req.ApplicationID, "application_type": req.ApplicationType}).Info(`Finished processing "create_application"`)

	case "destroy_application":
		req := &superkey.DestroyRequest{}
		err := msg.ParseTo(req)
		if err != nil {
			l.Log.WithFields(logrus.Fields{"org_id": orgIdHeader}).Errorf(`Error parsing "destroy_application" request "%s": %v`, string(msg.Value), err)
			return
		}

		if DisableDeletion == "true" {
			l.Log.Infof("Skipping destroy_application request: %v", msg.Value)
			return
		}

		l.Log.WithFields(logrus.Fields{"tenant_id": req.TenantID}).Info(`Processing "destroy_application" request`)

		destroyResources(req)

		l.Log.WithFields(logrus.Fields{"tenant_id": req.TenantID}).Info(`Finished processing "destroy_application" request`)

	default:
		l.Log.WithFields(logrus.Fields{"org_id": orgIdHeader}).Errorf(`Unknown event type "%s" received in the header, skipping request...`, eventType)
	}
}

func createResources(req *superkey.CreateRequest) {
	l.Log.WithFields(logrus.Fields{"tenant_id": req.TenantID, "source_id": req.SourceID, "application_id": req.ApplicationID}).Debugf("Forging request: %v", req)

	newApp, err := provider.Forge(req)
	if err != nil {
		l.Log.WithFields(logrus.Fields{"tenant_id": req.TenantID, "source_id": req.SourceID, "application_id": req.ApplicationID}).Errorf(`Tearing down Superkey request due to an error while forging the request \"%v\": %s`, req, err)

		errors := provider.TearDown(newApp)
		if len(errors) != 0 {
			for _, err := range errors {
				l.Log.WithFields(logrus.Fields{"tenant_id": req.TenantID, "source_id": req.SourceID, "application_id": req.ApplicationID}).Errorf(`Unable to tear down application: %s`, err)
			}
		}

		err := req.MarkSourceUnavailable(err, newApp)
		if err != nil {
			l.Log.WithFields(logrus.Fields{"tenant_id": req.TenantID, "source_id": req.SourceID, "application_id": req.ApplicationID}).Errorf(`Error while marking the source and application as "unavailable" in Sources: %s`, err)
		}

		return
	}

	l.Log.WithFields(logrus.Fields{"tenant_id": req.TenantID, "source_id": req.SourceID, "application_id": req.ApplicationID}).Debug("Finished forging request")

	err = newApp.CreateInSourcesAPI()
	if err != nil {
		l.Log.WithFields(logrus.Fields{"tenant_id": req.TenantID, "source_id": req.SourceID, "application_id": req.ApplicationID}).Errorf(`Error while creating or updating the resources in Sources: %s`, err)
		provider.TearDown(newApp)
	}
}

func destroyResources(req *superkey.DestroyRequest) {
	l.Log.WithFields(logrus.Fields{"tenant_id": req.TenantID}).Debugf(`Unforging request "%v"`, req)

	errors := provider.TearDown(superkey.ReconstructForgedApplication(req))
	if len(errors) != 0 {
		for _, err := range errors {
			l.Log.WithFields(logrus.Fields{"tenant_id": req.TenantID}).Errorf(`Error during teardown: %s"`, err)
		}
	}

	l.Log.WithFields(logrus.Fields{"tenant_id": req.TenantID}).Info("Finished destroying resources")
}

func initMetrics() {
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		err := http.ListenAndServe(fmt.Sprintf(":%d", conf.MetricsPort), nil)
		if err != nil {
			l.Log.Errorf("Metrics init error: %s", err)
		}
	}()
}

func initHealthCheck() {
	go func() {
		healthFile := "/tmp/healthy"
		// which endpoints can we hit and get a 200 from without any auth, these are the main 2
		// we need anyway.
		svcs := []string{"https://iam.amazonaws.com", "https://s3.amazonaws.com"}

		// custom http client with timeout
		client := http.Client{Timeout: 5 * time.Second}

		for {
			for _, svc := range svcs {
				resp, err := client.Get(svc)

				if err == nil && resp.StatusCode == 200 {
					// Copying to the bitbucket in order to gc the memory.
					_, err := io.Copy(io.Discard, resp.Body)
					if err != nil {
						l.Log.Errorf("Error discarding response body: %v", err)
					}

					err = resp.Body.Close()
					if err != nil {
						l.Log.Errorf("Error closing response body: %v", err)
					}

					// check if file exists, creating it if not (first run, or recovery)
					_, err = os.Stat(healthFile)
					if err != nil {
						_, err = os.Create(healthFile)
						if err != nil {
							l.Log.Errorf("Failed to touch healthcheck file")
						}
					}

				} else {
					l.Log.Warnf("Error hitting %s, err %v, removing %s", svc, err, healthFile)

					_, err = os.Stat(healthFile)
					// this is a bit complicated - err will be nil _if the file is there_, so we want
					// to remove only if the err is nil.
					if err == nil {
						_ = os.Remove(healthFile)
					}

					break
				}
			}

			time.Sleep(10 * time.Second)
		}
	}()
}
