package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

// Opt-in outbound webhooks: enabled only when WEBHOOK_URL is set.
// Delivery is best-effort and never blocks message handling.

const (
	webhookQueueSize = 100
	webhookAttempts  = 3
	webhookTimeout   = 5 * time.Second
)

var webhookBackoff = 500 * time.Millisecond

type webhookPayload struct {
	Event     string `json:"event"`
	Timestamp string `json:"timestamp"`
	ChatJID   string `json:"chat_jid"`
	MessageID string `json:"message_id"`
	Sender    string `json:"sender"`
	IsFromMe  bool   `json:"is_from_me"`
	Content   string `json:"content"`
	MediaType string `json:"media_type"`
}

type webhookDispatcher struct {
	url    string
	secret []byte
	client *http.Client
	queue  chan webhookPayload
}

var (
	webhookOnce sync.Once
	webhookDisp *webhookDispatcher
)

// newWebhookDispatcher returns (nil, nil) when disabled, an error when misconfigured.
func newWebhookDispatcher(rawURL, secret string) (*webhookDispatcher, error) {
	if rawURL == "" {
		return nil, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("WEBHOOK_URL must be a valid http or https URL")
	}
	if secret == "" {
		return nil, fmt.Errorf("WEBHOOK_SECRET is required when WEBHOOK_URL is set")
	}
	d := &webhookDispatcher{
		url:    rawURL,
		secret: []byte(secret),
		client: &http.Client{
			Timeout: webhookTimeout,
			// Never replay a signed POST to another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		queue:  make(chan webhookPayload, webhookQueueSize),
	}
	go d.run()
	return d, nil
}

func webhookSign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func (d *webhookDispatcher) enqueue(p webhookPayload) {
	select {
	case d.queue <- p:
	default:
		fmt.Printf("webhook: queue full, dropping %s\n", p.Event)
	}
}

func (d *webhookDispatcher) run() {
	for p := range d.queue {
		d.deliver(p)
	}
}

func (d *webhookDispatcher) deliver(p webhookPayload) {
	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	deliveryID := hex.EncodeToString(id)
	sig := webhookSign(d.secret, body)

	for attempt := 1; attempt <= webhookAttempts; attempt++ {
		req, err := http.NewRequest(http.MethodPost, d.url, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Signature", sig)
		req.Header.Set("X-Event", p.Event)
		req.Header.Set("X-Delivery-Id", deliveryID)
		resp, err := d.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		// Never log the URL (may embed a token) or the payload.
		fmt.Printf("webhook: delivery %s attempt %d/%d failed: %v\n", deliveryID, attempt, webhookAttempts, redactURLErr(err))
		if attempt < webhookAttempts {
			time.Sleep(webhookBackoff * time.Duration(attempt))
		}
	}
	fmt.Printf("webhook: delivery %s dropped after %d attempts\n", deliveryID, webhookAttempts)
}

// redactURLErr strips the URL that net/http embeds in client errors.
func redactURLErr(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

// emitWebhook is the single hook called from handleMessage for live messages.
func emitWebhook(msg *events.Message, chatJID, sender, content, mediaType string) {
	webhookOnce.Do(func() {
		d, err := newWebhookDispatcher(os.Getenv("WEBHOOK_URL"), os.Getenv("WEBHOOK_SECRET"))
		if err != nil {
			fmt.Printf("webhook: disabled: %v\n", err)
			return
		}
		webhookDisp = d
		if d != nil {
			fmt.Println("webhook: enabled")
		}
	})
	if webhookDisp == nil {
		return
	}
	event := "message.received"
	if msg.Info.IsFromMe {
		event = "message.sent"
	}
	webhookDisp.enqueue(webhookPayload{
		Event:     event,
		Timestamp: msg.Info.Timestamp.UTC().Format(time.RFC3339),
		ChatJID:   chatJID,
		MessageID: msg.Info.ID,
		Sender:    sender,
		IsFromMe:  msg.Info.IsFromMe,
		Content:   content,
		MediaType: mediaType,
	})
}
