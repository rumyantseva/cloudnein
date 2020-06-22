package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"gopkg.in/DataDog/dd-trace-go.v1/contrib/gorilla/mux"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	log := logger.Sugar().Named("grahovac").With("version", "v0.0.1")
	log.Info("The application is starting...")
	defer log.Info("The application is stopped.")

	c, err := statsd.New("127.0.0.1:8125")
	if err != nil {
		log.Fatalw("Couldn't connect to statsd", "err", err)
	}

	c.Namespace = "grahovac"
	defer func() {
		err := c.Close()
		log.Errorw("Error when closing statsd client", "err", err)
	}()

	tracer.Start(tracer.WithDebugMode(true))
	defer tracer.Stop()

	port := os.Getenv("PORT")
	if port == "" {
		log.Fatal("Business logic port is not set")
	}

	diagPort := os.Getenv("DIAG_PORT")
	if diagPort == "" {
		log.Fatal("Diagnostics port is not set")
	}

	log.Info("Configuration is read successfully")

	r := mux.NewRouter(mux.WithServiceName("grahovac.bl"))

	r.HandleFunc("/", func(
		w http.ResponseWriter, r *http.Request) {
		deep := r.URL.Query().Get("deep")
		if deep == "" {
			deep = "1"
		}

		options := []tracer.StartSpanOption{tracer.Tag("deep", deep)}

		spanCtx, err := tracer.Extract(tracer.HTTPHeadersCarrier(r.Header))
		if err == nil {
			options = append(options, tracer.ChildOf(spanCtx))
		}

		span := tracer.StartSpan("calculating_d", options...)
		defer span.Finish()

		sublog := log.With("dd.trace_id", span.Context().TraceID()).
			With("dd.span_id", span.Context().SpanID())

		sublog.Infof("The request received: %v", r.Header)

		d, err := strconv.Atoi(deep)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		sublog.Infof("The d is %d", d)

		if d >= 5 {
			w.WriteHeader(http.StatusOK)
			return
		}

		d++

		req, _ := http.NewRequest(
			http.MethodGet,
			fmt.Sprintf("http://127.0.0.1:%s/?deep=%d", port, d),
			nil,
		)

		err = tracer.Inject(span.Context(), tracer.HTTPHeadersCarrier(req.Header))
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		sublog.Infof("The request to send: %v", req.Header)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			//
		}

		if resp == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(resp.StatusCode)
	})

	server := http.Server{
		Addr:    net.JoinHostPort("", port),
		Handler: r,
	}

	diagRouter := mux.NewRouter(mux.WithServiceName("grahovac.diag"))

	healthCounter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "health_calls",
		Help: "The number of health calls",
	})
	prometheus.MustRegister(healthCounter)

	diagRouter.HandleFunc("/health", func(
		w http.ResponseWriter, _ *http.Request) {
		healthCounter.Inc()

		err := c.Incr("health_calls", []string{}, 1)
		if err != nil {
			log.Errorw("Can't increment health_calls", "err", err)
		}

		w.WriteHeader(http.StatusOK)
	})

	diagRouter.Handle("/prom", promhttp.Handler())

	diag := http.Server{
		Addr:    net.JoinHostPort("", diagPort),
		Handler: diagRouter,
	}

	shutdown := make(chan error, 2)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				// ToDo: convert buf to a proper string
				// buf := debug.Stack()
				// log.With("panictrace", buf).With("panic", r).Fatal("Got a panic")
			}
		}()

		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			shutdown <- err
		}
	}()

	go func() {
		err := diag.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			shutdown <- err
		}
	}()

	log.Info("The application is ready to listen to the user requests")

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	select {
	case x := <-interrupt:
		log.Infof("Received %s from OS", x.String())

	case err := <-shutdown:
		log.Errorf("Received an error from a server: %v", err)
	}

	timeout, cancelFunc := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFunc()

	err = diag.Shutdown(timeout)
	if err != nil {
		log.Errorf("Couldn't stop diagnostics server: %v", err)
	}

	err = server.Shutdown(timeout)
	if err != nil {
		log.Errorf("Couldn't stop business logic server: %v", err)
	}
}
