package proxmox

// inspired by https://github.com/openstack/golang-client/blob/master/openstack/session.go

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"time"
	"net/http"
	"net/http/httputil"
	"net/url"
)

var Debug = new(bool)

type ApiError struct {
    Code    int
    Message string
}

func (e *ApiError) Error() string {
    return fmt.Sprintf("Proxmox API error %d: %s", e.Code, e.Message)
}

const ApiErrorTooManyRedirections = 599

type Response struct {
	Resp *http.Response
	Body []byte
}

type Session struct {
	httpClient *http.Client
	ApiUrl     string
	AuthTicket string
	CsrfToken  string
	Headers    http.Header
}

func NewSession(configuration *Configuration, httpClient *http.Client) (session *Session, err error) {
	tlsConfig := &tls.Config{}
	if configuration.TlsInsecure {
		tlsConfig = &tls.Config{InsecureSkipVerify: true}
	}

	if httpClient == nil {
		// Only build a transport if we're also building the client
		tr := &http.Transport{
			TLSClientConfig:    tlsConfig,
			DisableCompression: true,
		}
		httpClient = &http.Client{Transport: tr, Timeout: time.Duration(HttpTimeout * time.Second)}
	}
	session = &Session{
		httpClient: httpClient,
		ApiUrl:     configuration.Url,
		AuthTicket: "",
		CsrfToken:  "",
		Headers:    http.Header{},
	}
	return
}

func ParamsToBody(params map[string]interface{}) (body []byte) {
	vals := url.Values{}
	for k, intrV := range params {
		var v string
		switch intrV.(type) {
		// Convert true/false bool to 1/0 string where Proxmox API can understand it.
		case bool:
			if intrV.(bool) {
				v = "1"
			} else {
				v = "0"
			}
		default:
			v = fmt.Sprintf("%v", intrV)
		}
		vals.Set(k, v)
	}
	body = bytes.NewBufferString(vals.Encode()).Bytes()
	return
}

func ResponseJSON(resp *http.Response) (jbody map[string]interface{}) {
	rbody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(fmt.Sprintf("error reading response body: %s", err))
	}
	if err = json.Unmarshal(rbody, &jbody); err != nil {
		return nil
	}
	return
}

func (s *Session) Login(username string, password string) (err error) {
	reqbody := ParamsToBody(map[string]interface{}{"username": username, "password": password})
	olddebug := *Debug
	*Debug = false // don't share passwords in debug log
	resp, err := s.Post("/access/ticket", nil, nil, &reqbody)
	*Debug = olddebug
	if err != nil {
		return
	}
	if resp == nil {
		return errors.New("Login error reading response")
	}
	dr, _ := httputil.DumpResponse(resp, true)
	jbody := ResponseJSON(resp)
	if jbody == nil || jbody["data"] == nil {
		return fmt.Errorf("Invalid login response:\n-----\n%s\n-----", dr)
	}
	dat := jbody["data"].(map[string]interface{})
	s.AuthTicket = dat["ticket"].(string)
	s.CsrfToken = dat["CSRFPreventionToken"].(string)
	return nil
}

func (s *Session) NewRequest(method, url string, headers *http.Header, body io.Reader) (req *http.Request, err error) {
	req, err = http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		req.Header = *headers
	}
	if s.AuthTicket != "" {
		req.Header.Add("Cookie", "PVEAuthCookie="+s.AuthTicket)
		req.Header.Add("CSRFPreventionToken", s.CsrfToken)
	}
	return
}

func (s *Session) Do(req *http.Request) (*http.Response, error) {
	// Add session headers
	for k := range s.Headers {
		req.Header.Set(k, s.Headers.Get(k))
	}

	if *Debug {
		d, _ := httputil.DumpRequestOut(req, true)
		log.Println(">>>>>>>>>> REQUEST:", string(d))
	}

	resp, err := s.httpClient.Do(req)

	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &ApiError{resp.StatusCode, resp.Status}
	}

	if *Debug {
		dr, _ := httputil.DumpResponse(resp, true)
		log.Println("<<<<<<<<<< RESULT:", string(dr))
	}

	return resp, nil
}

// Perform a simple get to an endpoint
func (s *Session) Request(
	method string,
	url string,
	params *url.Values,
	headers *http.Header,
	body *[]byte,
) (resp *http.Response, err error) {
	// add params to url here
	url = s.ApiUrl + url
	if params != nil {
		url = url + "?" + params.Encode()
	}

	// Get the body if one is present
	var buf io.Reader
	if body != nil {
		buf = bytes.NewReader(*body)
	}

	req, err := s.NewRequest(method, url, headers, buf)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")

	resp, err = s.Do(req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// Perform a simple get to an endpoint and unmarshall returned JSON
func (s *Session) RequestJSON(
	method string,
	url string,
	params *url.Values,
	headers *http.Header,
	body interface{},
	responseContainer interface{},
) (resp *http.Response, err error) {
	var bodyjson []byte
	if body != nil {
		bodyjson, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}

	// if headers == nil {
	// 	headers = &http.Header{}
	// 	headers.Add("Content-Type", "application/json")
	// }

	resp, err = s.Request(method, url, params, headers, &bodyjson)
	if err != nil {
		return nil, err
	}

	// err = util.CheckHTTPResponseStatusCode(resp)
	// if err != nil {
	// 	return nil, err
	// }

	rbody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.New("error reading response body")
	}
	if err = json.Unmarshal(rbody, &responseContainer); err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Session) Delete(
	url string,
	params *url.Values,
	headers *http.Header,
) (resp *http.Response, err error) {
	return s.Request("DELETE", url, params, headers, nil)
}

func (s *Session) Get(
	url string,
	params *url.Values,
	headers *http.Header,
) (resp *http.Response, err error) {
	return s.Request("GET", url, params, headers, nil)
}

func (s *Session) GetJSON(
	url string,
	params *url.Values,
	headers *http.Header,
	responseContainer interface{},
) (resp *http.Response, err error) {
	return s.RequestJSON("GET", url, params, headers, nil, responseContainer)
}

func (s *Session) Head(
	url string,
	params *url.Values,
	headers *http.Header,
) (resp *http.Response, err error) {
	return s.Request("HEAD", url, params, headers, nil)
}

func (s *Session) Post(
	url string,
	params *url.Values,
	headers *http.Header,
	body *[]byte,
) (resp *http.Response, err error) {
	if headers == nil {
		headers = &http.Header{}
		headers.Add("Content-Type", "application/x-www-form-urlencoded")
	}
	return s.Request("POST", url, params, headers, body)
}

func (s *Session) PostJSON(
	url string,
	params *url.Values,
	headers *http.Header,
	body interface{},
	responseContainer interface{},
) (resp *http.Response, err error) {
	return s.RequestJSON("POST", url, params, headers, body, responseContainer)
}

func (s *Session) Put(
	url string,
	params *url.Values,
	headers *http.Header,
	body *[]byte,
) (resp *http.Response, err error) {
	if headers == nil {
		headers = &http.Header{}
		headers.Add("Content-Type", "application/x-www-form-urlencoded")
	}
	return s.Request("PUT", url, params, headers, body)
}
