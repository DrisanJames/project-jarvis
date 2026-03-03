package main

import (
	"fmt"
	"os"

	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

func main() {
	secret := os.Getenv("SES_SMTP_SECRET")
	region := os.Getenv("SES_REGION")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "SES_SMTP_SECRET env var is required")
		os.Exit(1)
	}
	if region == "" {
		region = "us-west-1"
	}
	fmt.Println(mailing.DeriveSESSMTPPassword(secret, region))
}
