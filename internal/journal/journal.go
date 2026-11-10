// Package journal writes a JSONL audit trail, pushes phone alerts, and pings
// a dead-man heartbeat so silence means the bot is alive.
package journal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

type J struct {
	mu           sync.Mutex
	f            *os.File
	alertURL     string
	heartbeatURL string
	lastBeat     time.Time
	once         map[string]string // key -> UTC day already alerted
	hc           *http.Client
}

func Open(path, alertURL string) (*J, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &J{f: f, alertURL: alertURL, once: map[string]string{},
		hc: &http.Client{Timeout: 5 * time.Second}}, nil
}

// SetHeartbeat sets a dead-man URL (e.g. healthchecks.io). Beat pings it.
func (j *J) SetHeartbeat(url string) { j.heartbeatURL = url }

func (j *J) Event(kind string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	fields["kind"] = kind
	b, _ := json.Marshal(fields)
	j.mu.Lock()
	j.f.Write(append(b, '\n'))
	j.mu.Unlock()
	fmt.Println(string(b))
}

// Alert is for things that need you: it logs and pushes at high priority.
func (j *J) Alert(msg string) {
	j.Event("alert", map[string]any{"msg": msg})
	j.push("sigcup", msg, "high")
}

// AlertOnce alerts at most once per key per UTC day.
func (j *J) AlertOnce(key, msg string) {
	day := time.Now().UTC().Format("2006-01-02")
	j.mu.Lock()
	if j.once[key] == day {
		j.mu.Unlock()
		j.Event("alert_suppressed", map[string]any{"key": key, "msg": msg})
		return
	}
	j.once[key] = day
	j.mu.Unlock()
	j.Alert(msg)
}

// Notify is for things you read when convenient (the daily digest).
func (j *J) Notify(title, msg string) {
	j.Event("notify", map[string]any{"title": title, "msg": msg})
	j.push(title, msg, "low")
}

// Beat pings the heartbeat URL at most once a minute. Call it after each
// healthy loop; the monitor alerts you when the pings stop.
func (j *J) Beat() {
	if j.heartbeatURL == "" || time.Since(j.lastBeat) < time.Minute {
		return
	}
	j.lastBeat = time.Now()
	go func() {
		resp, err := j.hc.Get(j.heartbeatURL)
		if err == nil {
			resp.Body.Close()
		}
	}()
}

// push sends to ntfy-style endpoints. Title/Priority headers are ntfy's and
// harmless elsewhere.
func (j *J) push(title, msg, priority string) {
	if j.alertURL == "" {
		return
	}
	go func() {
		req, err := http.NewRequest("POST", j.alertURL, bytes.NewBufferString(msg))
		if err != nil {
			return
		}
		req.Header.Set("Title", title)
		req.Header.Set("Priority", priority)
		resp, err := j.hc.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
}

func (j *J) Close() { j.f.Close() }
