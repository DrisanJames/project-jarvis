package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type fakeSESDLQ struct {
	msgs    []sqstypes.Message
	deleted []string
	receive int
}

func (f *fakeSESDLQ) ReceiveMessage(_ context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.receive++
	n := int(in.MaxNumberOfMessages)
	if n > len(f.msgs) {
		n = len(f.msgs)
	}
	out := &sqs.ReceiveMessageOutput{Messages: f.msgs[:n]}
	f.msgs = f.msgs[n:]
	return out, nil
}

func (f *fakeSESDLQ) DeleteMessageBatch(_ context.Context, in *sqs.DeleteMessageBatchInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error) {
	out := &sqs.DeleteMessageBatchOutput{}
	for _, e := range in.Entries {
		f.deleted = append(f.deleted, aws.ToString(e.ReceiptHandle))
		out.Successful = append(out.Successful, sqstypes.DeleteMessageBatchResultEntry{Id: e.Id})
	}
	return out, nil
}

func (f *fakeSESDLQ) GetQueueAttributes(_ context.Context, _ *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	return &sqs.GetQueueAttributesOutput{Attributes: map[string]string{"ApproximateNumberOfMessages": "0"}}, nil
}

// A message is deleted ONLY when the webhook answers 2xx; a 5xx leaves it
// (visibility timeout returns it later). Envelope goes through byte-for-byte
// with the SNS headers.
func TestSESDLQDrainDeletesOnlyOn2xx(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		body := string(b[:n])
		got = append(got, body)
		if r.Header.Get("x-amz-sns-message-type") != "Notification" {
			t.Errorf("missing SNS header")
		}
		if strings.Contains(body, "FAIL") {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	f := &fakeSESDLQ{msgs: []sqstypes.Message{
		{MessageId: aws.String("m1"), ReceiptHandle: aws.String("r1"), Body: aws.String(`{"Type":"Notification","ok":1}`)},
		{MessageId: aws.String("m2"), ReceiptHandle: aws.String("r2"), Body: aws.String(`{"Type":"Notification","FAIL":1}`)},
		{MessageId: aws.String("m3"), ReceiptHandle: aws.String("r3"), Body: aws.String(`{"Type":"Notification","ok":3}`)},
	}}
	w := NewSESDLQDrainWorker(nil, nil, 8080).SetClient(f)
	w.webhookURL = srv.URL
	st, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Received != 3 || st.Replayed != 2 || st.Failed != 1 || st.Deleted != 2 {
		t.Fatalf("stats = %+v", st)
	}
	if strings.Join(f.deleted, ",") != "r1,r3" {
		t.Fatalf("deleted = %v, want r1,r3 (r2 must survive a 500)", f.deleted)
	}
	if len(got) != 3 {
		t.Fatalf("webhook saw %d posts", len(got))
	}
}

// A 503 (boot-time placeholder / ingest queue full) stops the pass at the
// first one: nothing is deleted, nothing further is received, and the
// readiness probe reports not-ready. Regression for :1163's boot tick, which
// churned 16,843 messages against a 503 webhook.
func TestSESDLQDrainStopsOn503(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts++
		http.Error(w, "ses webhook not ready", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	f := &fakeSESDLQ{}
	for i := 0; i < 30; i++ {
		f.msgs = append(f.msgs, sqstypes.Message{MessageId: aws.String("m"), ReceiptHandle: aws.String("r"), Body: aws.String(`{}`)})
	}
	w := NewSESDLQDrainWorker(nil, nil, 8080).SetClient(f)
	w.webhookURL = srv.URL
	if w.webhookReady(context.Background()) {
		t.Fatal("503 webhook reported ready")
	}
	st, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Received != 10 || st.Unavailable != 1 || st.Failed != 0 || st.Deleted != 0 || len(f.deleted) != 0 {
		t.Fatalf("stats = %+v deleted=%v; want one batch received, stop at first 503, nothing deleted", st, f.deleted)
	}
	if posts != 2 || f.receive != 1 || len(f.msgs) != 20 { // probe + the one replay that got the 503
		t.Fatalf("posts=%d receives=%d left=%d; want 2/1/20", posts, f.receive, len(f.msgs))
	}
	// A wired handler answers 400 to an empty envelope — that IS ready.
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "invalid envelope", 400) }))
	defer ready.Close()
	w.webhookURL = ready.URL
	if !w.webhookReady(context.Background()) {
		t.Fatal("400 webhook reported not ready")
	}
}

// Kill switch and per-tick cap are honoured.
func TestSESDLQDrainMaxPerTick(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	f := &fakeSESDLQ{}
	for i := 0; i < 25; i++ {
		f.msgs = append(f.msgs, sqstypes.Message{MessageId: aws.String("m"), ReceiptHandle: aws.String("r"), Body: aws.String(`{}`)})
	}
	w := NewSESDLQDrainWorker(nil, nil, 8080).SetClient(f)
	w.webhookURL = srv.URL
	w.maxPerTick = 20
	st, _ := w.RunOnce(context.Background())
	if st.Received != 20 || len(f.msgs) != 5 {
		t.Fatalf("cap not honoured: received %d, left %d", st.Received, len(f.msgs))
	}
	t.Setenv("SES_DLQ_DRAIN_DISABLED", "1")
	if !sesDLQDisabled() {
		t.Fatal("kill switch not read")
	}
}
