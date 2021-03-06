/*
Package firego is a REST client for Firebase (https://firebase.com).
*/
package firego

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	_url "net/url"
	"strings"
	"sync"
	"time"
)

// TimeoutDuration is the length of time any request will have to establish
// a connection and receive headers from Firebase before returning
// an ErrTimeout error.
var TimeoutDuration = 30 * time.Second

var defaultRedirectLimit = 30

// ErrTimeout is an error type is that is returned if a request
// exceeds the TimeoutDuration configured.
type ErrTimeout struct {
	error
}

// query parameter constants
const (
	authParam         = "auth"
	formatParam       = "format"
	shallowParam      = "shallow"
	orderByParam      = "orderBy"
	startAtParam      = "startAt"
	endAtParam        = "endAt"
	formatVal         = "export"
	limitToFirstParam = "limitToFirst"
	limitToLastParam  = "limitToLast"
)

// Firebase represents a location in the cloud.
type Firebase struct {
	url    string
	params _url.Values
	client *http.Client

	watchMtx     sync.Mutex
	watching     bool
	stopWatching chan struct{}
}

// Auth sets the custom Firebase token used to authenticate to Firebase.
func (fb *Firebase) Auth(token string) {
	fb.params.Set(authParam, token)
}

// Unauth removes the current token being used to authenticate to Firebase.
func (fb *Firebase) Unauth() {
	fb.params.Del(authParam)
}

// Push creates a reference to an auto-generated child location.
func (fb *Firebase) Push(v interface{}) (*Firebase, error) {
	bytes, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	bytes, err = fb.doRequest("POST", bytes)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(bytes, &m); err != nil {
		return nil, err
	}
	return &Firebase{
		url:    fb.url + "/" + m["name"],
		client: fb.client,
	}, err
}

// Remove the Firebase reference from the cloud.
func (fb *Firebase) Remove() error {
	_, err := fb.doRequest("DELETE", nil)
	if err != nil {
		return err
	}
	return nil
}

// Set the value of the Firebase reference.
func (fb *Firebase) Set(v interface{}) error {
	bytes, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fb.doRequest("PUT", bytes)
	return err
}

// Update the specific child with the given value.
func (fb *Firebase) Update(v interface{}) error {
	bytes, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fb.doRequest("PATCH", bytes)
	return err
}

// Value gets the value of the Firebase reference.
func (fb *Firebase) Value(v interface{}) error {
	bytes, err := fb.doRequest("GET", nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, v)
}

func sanitizeURL(url string) string {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		url = "https://" + url
	}

	if strings.HasSuffix(url, "/") {
		url = url[:len(url)-1]
	}

	return url
}

// Preserve headers on redirect.
//
// Reference https://github.com/golang/go/issues/4800
func redirectPreserveHeaders(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		// No redirects
		return nil
	}

	if len(via) > defaultRedirectLimit {
		return fmt.Errorf("%d consecutive requests(redirects)", len(via))
	}

	// mutate the subsequent redirect requests with the first Header
	for key, val := range via[0].Header {
		req.Header[key] = val
	}
	return nil
}

// New creates a new Firebase reference,
// if client is nil, http.DefaultClient is used.
func New(url string, client *http.Client) *Firebase {

	if client == nil {
		var tr *http.Transport
		tr = &http.Transport{
			DisableKeepAlives: true, // https://code.google.com/p/go/issues/detail?id=3514
			Dial: func(network, address string) (net.Conn, error) {
				start := time.Now()
				c, err := net.DialTimeout(network, address, TimeoutDuration)
				tr.ResponseHeaderTimeout = TimeoutDuration - time.Since(start)
				return c, err
			},
		}

		client = &http.Client{
			Transport:     tr,
			CheckRedirect: redirectPreserveHeaders,
		}
	}

	return &Firebase{
		url:          sanitizeURL(url),
		params:       _url.Values{},
		client:       client,
		stopWatching: make(chan struct{}),
	}
}

// String returns the string representation of the
// Firebase reference.
func (fb *Firebase) String() string {
	path := fb.url + "/.json"

	if len(fb.params) > 0 {
		path += "?" + fb.params.Encode()
	}
	return path
}

// Child creates a new Firebase reference for the requested
// child with the same configuration as the parent.
func (fb *Firebase) Child(child string) *Firebase {
	c := fb.copy()
	c.url = c.url + "/" + child
	return c
}

func (fb *Firebase) copy() *Firebase {
	c := &Firebase{
		url:          fb.url,
		params:       _url.Values{},
		client:       fb.client,
		stopWatching: make(chan struct{}),
	}

	// making sure to manually copy the map items into a new
	// map to avoid modifying the map reference.
	for k, v := range fb.params {
		c.params[k] = v
	}
	return c
}

func (fb *Firebase) makeRequest(method string, body []byte) (*http.Request, error) {
	return http.NewRequest(method, fb.String(), bytes.NewReader(body))
}

func (fb *Firebase) doRequest(method string, body []byte) ([]byte, error) {
	req, err := fb.makeRequest(method, body)
	if err != nil {
		return nil, err
	}

	resp, err := fb.client.Do(req)
	switch err := err.(type) {
	default:
		return nil, err
	case nil:
		// carry on

	case *_url.Error:
		// `http.Client.Do` will return a `url.Error` that wraps a `net.Error`
		// when exceeding it's `Transport`'s `ResponseHeadersTimeout`
		e1, ok := err.Err.(net.Error)
		if ok && e1.Timeout() {
			return nil, ErrTimeout{err}
		}

		return nil, err

	case net.Error:
		// `http.Client.Do` will return a `net.Error` directly when Dial times
		// out, or when the Client's RoundTripper otherwise returns an err
		if err.Timeout() {
			return nil, ErrTimeout{err}
		}

		return nil, err
	}

	defer resp.Body.Close()
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/200 != 1 {
		return nil, errors.New(string(respBody))
	}
	return respBody, nil
}
