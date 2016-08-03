// Package gcm provides send and receive GCM functionality.
package gcm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	log "github.com/Sirupsen/logrus"
)

const (
	httpAddress = "https://gcm-http.googleapis.com/gcm/send"
)

// HTTPMessage defines a downstream GCM HTTP message.
type HTTPMessage struct {
	To                    string        `json:"to,omitempty"`
	RegistrationIDs       []string      `json:"registration_ids,omitempty"`
	CollapseKey           string        `json:"collapse_key,omitempty"`
	Priority              string        `json:"priority,omitempty"`
	ContentAvailable      bool          `json:"content_available,omitempty"`
	DelayWhileIdle        bool          `json:"delay_while_idle,omitempty"`
	TimeToLive            *uint         `json:"time_to_live,omitempty"`
	RestrictedPackageName string        `json:"restricted_package_name,omitempty"`
	DryRun                bool          `json:"dry_run,omitempty"`
	Data                  Data          `json:"data,omitempty"`
	Notification          *Notification `json:"notification,omitempty"`
}

// HTTPResponse is the GCM connection server response to an HTTP downstream message.
type HTTPResponse struct {
	MulticastID  int64    `json:"multicast_id,omitempty"`
	Success      uint     `json:"success,omitempty"`
	Failure      uint     `json:"failure,omitempty"`
	CanonicalIds uint     `json:"canonical_ids,omitempty"`
	Results      []Result `json:"results,omitempty"`
	MessageID    uint     `json:"message_id,omitempty"`
	Error        string   `json:"error,omitempty"`
}

// Result represents the status of a processed HTTP message.
type Result struct {
	MessageID      string `json:"message_id,omitempty"`
	RegistrationID string `json:"registration_id,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Used to compute results for multicast messages with retries.
type multicastResultsState map[string]*Result

// httpClient is an interface to stub the http client in tests.
type httpClient interface {
	Send(m HTTPMessage) (*HTTPResponse, error)
	GetRetryAfter() string
}

// httpGCMClient is a client for the Gcm Http Connection Server.
type httpGCMClient struct {
	GCMURL     string
	apiKey     string
	httpClient *http.Client
	retryAfter string
	debug      bool
}

// backoffProvider defines an interface for backoff.
type backoffProvider interface {
	sendAnother() bool
	setMin(min time.Duration)
	wait()
}

// newHTTPGCMClient creates a new client for handling GCM HTTP requests.
func newHTTPGCMClient(apiKey string, debug bool) *httpGCMClient {
	return &httpGCMClient{
		GCMURL:     httpAddress,
		apiKey:     apiKey,
		httpClient: &http.Client{},
		retryAfter: "0",
		debug:      debug,
	}
}

// GetRetryAfter gets the value of the retry after header if present.
func (c httpGCMClient) GetRetryAfter() string {
	return c.retryAfter
}

// Send sends an HTTP message using exponential backoff, handling multicast replies.
func (c *httpGCMClient) Send(m HTTPMessage) (*HTTPResponse, error) {
	b := newExponentialBackoff()
	// TODO(silvano): check this with responses for topic/notification group.
	gcmResp := &HTTPResponse{}
	var multicastID int64
	targets, err := messageTargetAsStringsArray(m)
	if err != nil {
		return gcmResp, fmt.Errorf("error extracting target from message: %v", err)
	}

	// Make a copy of the targets to keep track of results during retries.
	localTo := make([]string, len(targets))
	copy(localTo, targets)
	resultsState := &multicastResultsState{}
	for b.sendAnother() {
		gcmResp, err = c.sendHTTP(m)
		if err != nil {
			return gcmResp, fmt.Errorf("error sending request to GCM HTTP server: %v", err)
		}
		if len(gcmResp.Results) > 0 {
			doRetry, toRetry, err := checkResults(gcmResp.Results, localTo, *resultsState)
			multicastID = gcmResp.MulticastID
			if err != nil {
				return gcmResp, fmt.Errorf("error checking GCM results: %v", err)
			}
			if doRetry {
				retryAfter, err := time.ParseDuration(c.GetRetryAfter())
				if err != nil {
					b.setMin(retryAfter)
				}
				localTo = make([]string, len(toRetry))
				copy(localTo, toRetry)
				if m.RegistrationIDs != nil {
					m.RegistrationIDs = toRetry
				}
				b.wait()
				continue
			} else {
				break
			}
		} else {
			break
		}
	}

	// if it was multicast, reconstruct response in case there have been retries
	if len(targets) > 1 {
		gcmResp = buildRespForMulticast(targets, *resultsState, multicastID)
	}

	return gcmResp, nil
}

// sendHTTP sends a request to GCM HTTP server and parses the response.
func (c *httpGCMClient) sendHTTP(m HTTPMessage) (*HTTPResponse, error) {
	bs, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("error marshalling message: %v", err)
	}
	if c.debug {
		log.WithField("http request", string(bs)).Debug("gcm http request")
	}

	req, err := http.NewRequest("POST", c.GCMURL, bytes.NewReader(bs))
	if err != nil {
		return nil, fmt.Errorf("error creating request: %v", err)
	}

	// Add required headers.
	req.Header.Add(http.CanonicalHeaderKey("Content-Type"), "application/json")
	req.Header.Add(http.CanonicalHeaderKey("Authorization"), fmt.Sprintf("key=%v", c.apiKey))

	httpResp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error sending request to HTTP connection server>%v", err)
	}

	gcmResp := &HTTPResponse{}
	body, err := ioutil.ReadAll(httpResp.Body)
	defer httpResp.Body.Close()
	if err != nil {
		return gcmResp, fmt.Errorf("error reading http response body: %v", err)
	}

	if c.debug {
		log.WithField("http reply", string(body)).Debug("gcm http reply")
	}
	err = json.Unmarshal(body, &gcmResp)
	if err != nil {
		return gcmResp, fmt.Errorf("error unmarshaling json from body: %v %s", err, string(body))
	}

	// TODO(silvano): this is assuming that the header contains seconds instead of a date, need to check
	c.retryAfter = httpResp.Header.Get(http.CanonicalHeaderKey("Retry-After"))
	return gcmResp, nil
}

// buildRespForMulticast builds the final response for a multicast message, in case there have been
// retries for subsets of the original recipients.
func buildRespForMulticast(to []string, mrs multicastResultsState, mid int64) *HTTPResponse {
	resp := &HTTPResponse{}
	resp.MulticastID = mid
	resp.Results = make([]Result, len(to))
	for i, regID := range to {
		result, ok := mrs[regID]
		if !ok {
			continue
		}
		resp.Results[i] = *result
		if result.MessageID != "" {
			resp.Success++
		} else if result.Error != "" {
			resp.Failure++
		}
		if result.RegistrationID != "" {
			resp.CanonicalIds++
		}
	}
	return resp
}

// messageTargetAsStringsArray transforms the recipient in an array of strings if needed.
func messageTargetAsStringsArray(m HTTPMessage) ([]string, error) {
	if m.RegistrationIDs != nil {
		return m.RegistrationIDs, nil
	} else if m.To != "" {
		target := []string{m.To}
		return target, nil
	}
	target := []string{}
	return target, fmt.Errorf("cannot find any valid target field in message")
}

// checkResults determines which errors can be retried in the multicast send.
func checkResults(gcmResults []Result, recipients []string,
	resultsState multicastResultsState) (doRetry bool, toRetry []string, err error) {
	doRetry = false
	toRetry = []string{}
	for i := 0; i < len(gcmResults); i++ {
		result := gcmResults[i]
		regID := recipients[i]
		resultsState[regID] = &result
		if result.Error != "" {
			if retryableErrors[result.Error] {
				toRetry = append(toRetry, regID)
				if doRetry == false {
					doRetry = true
				}
			}
		}
	}
	return doRetry, toRetry, nil
}
