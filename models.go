package main

import (
	"bytes"
	"crypto/md5"
	"encoding/gob"
	"fmt"
	"io"
	"net/http"

	log "github.com/Sirupsen/logrus"
	"github.com/elazarl/goproxy"
	"io/ioutil"
)

// DBClient provides access to cache, http client and configuration
type DBClient struct {
	cache Cache
	http  *http.Client
	cfg   *Configuration
}

// request holds structure for request
type request struct {
	details requestDetails
}

var emptyResp = &http.Response{}

// requestDetails stores information about request, it's used for creating unique hash and also as a payload structure
type requestDetails struct {
	Path        string              `json:"path"`
	Method      string              `json:"method"`
	Destination string              `json:"destination"`
	Scheme      string              `json:"scheme"`
	Query       string              `json:"query"`
	Body        string              `json:"body"`
	RemoteAddr  string              `json:"remoteAddr"`
	Headers     map[string][]string `json:"headers"`
}

func (r *request) concatenate() string {
	var buffer bytes.Buffer

	buffer.WriteString(r.details.Destination)
	buffer.WriteString(r.details.Path)
	buffer.WriteString(r.details.Method)
	buffer.WriteString(r.details.Query)
	buffer.WriteString(r.details.Body)

	return buffer.String()
}

// hash returns unique hash key for request
func (r *request) hash() string {
	h := md5.New()
	io.WriteString(h, r.concatenate())
	return fmt.Sprintf("%x", h.Sum(nil))
}

// res structure hold response body from external service, body is not decoded and is supposed
// to be bytes, however headers should provide all required information for later decoding
// by the client.
type response struct {
	Status  int                 `json:"status"`
	Body    string              `json:"body"`
	Headers map[string][]string `json:"headers"`
}

// Payload structure holds request and response structure
type Payload struct {
	Response response       `json:"response"`
	Request  requestDetails `json:"request"`
	ID       string         `json:"id"`
}

// encode method encodes all exported Payload fields to bytes
func (p *Payload) encode() ([]byte, error) {
	buf := new(bytes.Buffer)
	enc := gob.NewEncoder(buf)
	err := enc.Encode(p)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodePayload decodes supplied bytes into Payload structure
func decodePayload(data []byte) (*Payload, error) {
	var p *Payload
	buf := bytes.NewBuffer(data)
	dec := gob.NewDecoder(buf)
	err := dec.Decode(&p)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// recordRequest saves request for later playback
func (d *DBClient) captureRequest(req *http.Request) (*http.Response, error) {

	// this is mainly for testing, since when you create
	if req.Body == nil {
		req.Body = ioutil.NopCloser(bytes.NewBuffer([]byte("")))
	}

	reqBody, err := ioutil.ReadAll(req.Body)

	if err != nil {
		log.WithFields(log.Fields{
			"error": err.Error(),
		}).Error("Got error when reading request body")
	}
	log.WithFields(log.Fields{
		"body": string(reqBody),
	}).Info("got request body")
	req.Body = ioutil.NopCloser(bytes.NewBuffer(reqBody))

	// forwarding request
	resp, err := d.doRequest(req)

	if err == nil {
		respBody, err := extractBody(resp)
		if err != nil {
			// copying the response body did not work
			if err != nil {
				log.WithFields(log.Fields{
					"error": err.Error(),
				}).Error("Failed to copy response body.")
			}
		}

		// saving response body with request/response meta to cache
		d.save(req, reqBody, resp, respBody)
	}

	// return new response or error here
	return resp, err
}

func copyBody(body io.ReadCloser) (resp1, resp2 io.ReadCloser, err error) {
	var buf bytes.Buffer
	if _, err = buf.ReadFrom(body); err != nil {
		return nil, nil, err
	}
	if err = body.Close(); err != nil {
		return nil, nil, err
	}
	return ioutil.NopCloser(&buf), ioutil.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

func extractBody(resp *http.Response) (extract []byte, err error) {
	save := resp.Body
	savecl := resp.ContentLength

	save, resp.Body, err = copyBody(resp.Body)

	if err != nil {
		return
	}
	defer resp.Body.Close()
	extract, err = ioutil.ReadAll(resp.Body)

	resp.Body = save
	resp.ContentLength = savecl
	if err != nil {
		return nil, err
	}
	return extract, nil
}

// doRequest performs original request and returns response that should be returned to client and error (if there is one)
func (d *DBClient) doRequest(request *http.Request) (*http.Response, error) {
	// We can't have this set. And it only contains "/pkg/net/http/" anyway
	request.RequestURI = ""

	if d.cfg.middleware != "" {
		var payload Payload

		c := NewConstructor(request, payload)
		c.ApplyMiddleware(d.cfg.middleware)

		request = c.reconstructRequest()

	}

	resp, err := d.http.Do(request)

	if err != nil {
		log.WithFields(log.Fields{
			"error":  err.Error(),
			"host":   request.Host,
			"method": request.Method,
			"path":   request.URL.Path,
		}).Error("Could not forward request.")
		return nil, err
	}

	log.WithFields(log.Fields{
		"host":   request.Host,
		"method": request.Method,
		"path":   request.URL.Path,
	}).Info("Response got successfuly!")

	resp.Header.Set("hoverfly", "Was-Here")
	return resp, nil

}

// save gets request fingerprint, extracts request body, status code and headers, then saves it to cache
func (d *DBClient) save(req *http.Request, reqBody []byte, resp *http.Response, respBody []byte) {
	// record request here
	key := getRequestFingerprint(req, reqBody)

	if resp == nil {
		resp = emptyResp
	} else {
		responseObj := response{
			Status:  resp.StatusCode,
			Body:    string(respBody),
			Headers: resp.Header,
		}

		log.WithFields(log.Fields{
			"path":          req.URL.Path,
			"rawQuery":      req.URL.RawQuery,
			"requestMethod": req.Method,
			"bodyLen":       len(reqBody),
			"destination":   req.Host,
			"hashKey":       key,
		}).Info("Capturing")

		requestObj := requestDetails{
			Path:        req.URL.Path,
			Method:      req.Method,
			Destination: req.Host,
			Scheme:      req.URL.Scheme,
			Query:       req.URL.RawQuery,
			Body:        string(reqBody),
			RemoteAddr:  req.RemoteAddr,
			Headers:     req.Header,
		}

		payload := Payload{
			Response: responseObj,
			Request:  requestObj,
			ID:       key,
		}

		bts, err := payload.encode()
		if err != nil {
			log.WithFields(log.Fields{
				"error": err.Error(),
			}).Error("Failed to serialize payload")
		} else {
			d.cache.Set([]byte(key), bts)
		}
	}
}

// getRequestFingerprint returns request hash
func getRequestFingerprint(req *http.Request, requestBody []byte) string {
	details := requestDetails{
		Path:        req.URL.Path,
		Method:      req.Method,
		Destination: req.Host,
		Query:       req.URL.RawQuery,
		Body:        string(requestBody),
	}

	r := request{details: details}
	return r.hash()
}

// getResponse returns stored response from cache
func (d *DBClient) getResponse(req *http.Request) *http.Response {

	reqBody, err := ioutil.ReadAll(req.Body)

	if err != nil {
		log.WithFields(log.Fields{
			"error": err.Error(),
		}).Error("Got error when reading request body")
	}

	key := getRequestFingerprint(req, reqBody)

	payloadBts, err := d.cache.Get([]byte(key))

	if err == nil {
		// getting cache response
		payload, err := decodePayload(payloadBts)
		if err != nil {
			log.WithFields(log.Fields{
				"error": err.Error(),
			}).Error("Failed to decode payload")
			return nil
		}

		c := NewConstructor(req, *payload)

		if d.cfg.middleware != "" {
			_ = c.ApplyMiddleware(d.cfg.middleware)
		}

		response := c.reconstructResponse()

		log.WithFields(log.Fields{
			"key":        key,
			"status":     payload.Response.Status,
			"bodyLength": response.ContentLength,
		}).Info("Response found, returning")

		return response

	}

	log.WithFields(log.Fields{
		"error":       err.Error(),
		"query":       req.URL.RawQuery,
		"path":        req.URL.RawPath,
		"destination": req.Host,
		"method":      req.Method,
	}).Warn("Failed to retrieve response from cache")
	// return error? if we return nil - proxy forwards request to original destination
	return goproxy.NewResponse(req,
		goproxy.ContentTypeText, http.StatusPreconditionFailed,
		"Coudldn't find recorded request, please record it first!")

}

// modifyRequestResponse modifies outgoing request and then modifies incoming response, neither request nor response
// is saved to cache.
func (d *DBClient) modifyRequestResponse(req *http.Request, middleware string) (*http.Response, error) {

	// modifying request
	resp, err := d.doRequest(req)

	if err != nil {
		return nil, err
	}

	// preparing payload
	bodyBytes, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		log.WithFields(log.Fields{
			"error":      err.Error(),
			"middleware": middleware,
		}).Error("Failed to read response body after sending modified request")
		return nil, err
	}

	r := response{
		Status:  resp.StatusCode,
		Body:    string(bodyBytes),
		Headers: resp.Header,
	}
	payload := Payload{Response: r}

	c := NewConstructor(req, payload)
	// applying middleware to modify response
	err = c.ApplyMiddleware(middleware)

	if err != nil {
		return nil, err
	}

	newResponse := c.reconstructResponse()

	log.WithFields(log.Fields{
		"status":     newResponse.StatusCode,
		"middleware": middleware,
	}).Info("Response modified, returning")

	return newResponse, nil

}
