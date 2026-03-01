package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/ignite/sparkpost-monitor/internal/tracking"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	queueURL := os.Getenv("SQS_TRACKING_QUEUE_URL")
	if queueURL == "" {
		log.Fatal("SQS_TRACKING_QUEUE_URL is required")
	}

	awsCfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("aws config: %v", err)
	}

	sqsClient := sqs.NewFromConfig(awsCfg)
	pub := tracking.NewPublisher(sqsClient, queueURL)
	handler := tracking.NewHandler(pub)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      handler.Routes(),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("tracking service listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down tracking service...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
