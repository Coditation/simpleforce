package simpleforce

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

const (
	DefaultAPIVersion = "43.0"
	DefaultClientID   = "simpleforce"
	DefaultURL        = "https://login.salesforce.com"

	logPrefix = "[simpleforce]"
)

// Client is the main instance to access salesforce.
type Client struct {
	sessionID string
	user      struct {
		id       string
		name     string
		fullName string
		email    string
	}
	clientID      string
	apiVersion    string
	baseURL       string
	instanceURL   string
	useToolingAPI bool
	httpClient    *http.Client
}

// QueryResult holds the response data from an SOQL query.
type QueryResult struct {
	TotalSize      int       `json:"totalSize"`
	Done           bool      `json:"done"`
	NextRecordsURL string    `json:"nextRecordsUrl"`
	Records        []SObject `json:"records"`
}

// Token holdes the response frome response token
type Token struct {
	Id               string `json:"id,omitempty"`
	IssuedAt         string `json:"issued_at,omitempty"`
	Scope            string `json:"scope,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	InstanceUrl      string `json:"instance_url,omitempty"`
	Signature        string `json:"signature,omitempty"`
	AccessToken      string `json:"access_token,omitempty"`
}

// Expose sid to save in admin settings
func (client *Client) GetSid() (sid string) {
	return client.sessionID
}

//Expose Loc to save in admin settings
func (client *Client) GetLoc() (loc string) {
	return client.instanceURL
}

// Set SID and Loc as a means to log in without LoginPassword
func (client *Client) SetSidLoc(sid string, loc string) {
	client.sessionID = sid
	client.instanceURL = loc
}

// Query runs an SOQL query. q could either be the SOQL string or the nextRecordsURL.
func (client *Client) Query(q string) (*QueryResult, error) {
	if !client.isLoggedIn() {
		return nil, ERR_AUTHENTICATION
	}

	var u string
	if strings.HasPrefix(q, "/services/data") {
		// q is nextRecordsURL.
		u = fmt.Sprintf("%s%s", client.instanceURL, q)
	} else {
		// q is SOQL.
		formatString := "%s/services/data/v%s/query?q=%s"
		baseURL := client.instanceURL
		if client.useToolingAPI {
			formatString = strings.Replace(formatString, "query", "tooling/query", -1)
		}
		u = fmt.Sprintf(formatString, baseURL, client.apiVersion, url.PathEscape(q))
	}

	data, code, err := client.httpRequest("GET", u, nil)
	if err != nil {
		log.Println(logPrefix, "HTTP GET request failed:", u)
		return nil, err
	}

	if RetryLogic(code) {
		return nil, ERR_RETRY
	}

	var result QueryResult
	err = json.Unmarshal(data, &result)
	if err != nil {
		return nil, ERR_FAILURE
	}

	// Reference to client is needed if the object will be further used to do online queries.
	for idx := range result.Records {
		result.Records[idx].setClient(client)
	}

	return &result, nil
}

// SObject creates an SObject instance with provided type name and associate the SObject with the client.
func (client *Client) SObject(typeName ...string) *SObject {
	obj := &SObject{}
	obj.setClient(client)
	if typeName != nil {
		obj.setType(typeName[0])
	}
	return obj
}

// isLoggedIn returns if the login to salesforce is successful.
func (client *Client) isLoggedIn() bool {
	return client.sessionID != ""
}

// LoginPassword signs into salesforce using password. token is optional if trusted IP is configured.
// Ref: https://developer.salesforce.com/docs/atlas.en-us.214.0.api_rest.meta/api_rest/intro_understanding_username_password_oauth_flow.htm
// Ref: https://developer.salesforce.com/docs/atlas.en-us.214.0.api.meta/api/sforce_api_calls_login.htm
func (client *Client) LoginPassword(username, password, token string) error {
	// Use the SOAP interface to acquire session ID with username, password, and token.
	// Do not use REST interface here as REST interface seems to have strong checking against client_id, while the SOAP
	// interface allows a non-exist placeholder client_id to be used.
	soapBody := `<?xml version="1.0" encoding="utf-8" ?>
        <env:Envelope
                xmlns:xsd="http://www.w3.org/2001/XMLSchema"
                xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
                xmlns:env="http://schemas.xmlsoap.org/soap/envelope/"
                xmlns:urn="urn:partner.soap.sforce.com">
            <env:Header>
                <urn:CallOptions>
                    <urn:client>%s</urn:client>
                    <urn:defaultNamespace>sf</urn:defaultNamespace>
                </urn:CallOptions>
            </env:Header>
            <env:Body>
                <n1:login xmlns:n1="urn:partner.soap.sforce.com">
                    <n1:username>%s</n1:username>
                    <n1:password>%s%s</n1:password>
                </n1:login>
            </env:Body>
        </env:Envelope>`
	soapBody = fmt.Sprintf(soapBody, client.clientID, username, html.EscapeString(password), token)

	url := fmt.Sprintf("%s/services/Soap/u/%s", client.baseURL, client.apiVersion)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(soapBody))
	if err != nil {
		log.Println(logPrefix, "error occurred creating request,", err)
		return err
	}
	req.Header.Add("Content-Type", "text/xml")
	req.Header.Add("charset", "UTF-8")
	req.Header.Add("SOAPAction", "login")

	resp, err := client.httpClient.Do(req)
	if err != nil {
		log.Println(logPrefix, "error occurred submitting request,", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Println(logPrefix, "request failed,", resp.StatusCode)
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		newStr := buf.String()
		log.Println(logPrefix, "Failed resp.body: ", newStr)
		theError := ParseSalesforceError(resp.StatusCode, buf.Bytes())
		return theError
	}

	respData, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		log.Println(logPrefix, "error occurred reading response data,", err)
	}

	var loginResponse struct {
		XMLName      xml.Name `xml:"Envelope"`
		ServerURL    string   `xml:"Body>loginResponse>result>serverUrl"`
		SessionID    string   `xml:"Body>loginResponse>result>sessionId"`
		UserID       string   `xml:"Body>loginResponse>result>userId"`
		UserEmail    string   `xml:"Body>loginResponse>result>userInfo>userEmail"`
		UserFullName string   `xml:"Body>loginResponse>result>userInfo>userFullName"`
		UserName     string   `xml:"Body>loginResponse>result>userInfo>userName"`
	}

	err = xml.Unmarshal(respData, &loginResponse)
	if err != nil {
		log.Println(logPrefix, "error occurred parsing login response,", err)
		return err
	}

	// Now we should all be good and the sessionID can be used to talk to salesforce further.
	client.sessionID = loginResponse.SessionID
	client.instanceURL = parseHost(loginResponse.ServerURL)
	client.user.id = loginResponse.UserID
	client.user.name = loginResponse.UserName
	client.user.email = loginResponse.UserEmail
	client.user.fullName = loginResponse.UserFullName

	log.Println(logPrefix, "User", client.user.name, "authenticated.")
	return nil
}

// httpRequest executes an HTTP request to the salesforce server and returns the response data in byte buffer.
func (client *Client) httpRequest(method, url string, body io.Reader) ([]byte, int, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", client.sessionID))
	req.Header.Add("Content-Type", "application/json")

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Println(logPrefix, "request failed,", resp.StatusCode)
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		theError := ParseSalesforceError(resp.StatusCode, buf.Bytes())
		return nil, resp.StatusCode, theError
	}
	rBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, http.StatusInternalServerError, ERR_FAILURE
	}
	return rBody, resp.StatusCode, nil
}

// makeURL generates a REST API URL based on baseURL, APIVersion of the client.
func (client *Client) makeURL(req string) string {
	client.apiVersion = strings.Replace(client.apiVersion, "v", "", -1)
	retURL := fmt.Sprintf("%s/services/data/v%s/%s", client.instanceURL, client.apiVersion, req)
	return retURL
}

// NewClient creates a new instance of the client.
func NewClient(url, clientID, apiVersion string) *Client {
	client := &Client{
		apiVersion: apiVersion,
		baseURL:    url,
		clientID:   clientID,
		httpClient: &http.Client{},
	}

	// Append "/" to the end of baseURL if not yet.
	if !strings.HasSuffix(client.baseURL, "/") {
		client.baseURL = client.baseURL + "/"
	}
	return client
}

func (client *Client) SetHttpClient(c *http.Client) {
	client.httpClient = c
}

// DownloadFile downloads a file based on the REST API path given. Saves to filePath.
func (client *Client) DownloadFile(contentVersionID string, filepath string) error {

	apiPath := fmt.Sprintf("/services/data/v%s/sobjects/ContentVersion/%s/VersionData", client.apiVersion, contentVersionID)

	baseURL := strings.TrimRight(client.baseURL, "/")
	url := fmt.Sprintf("%s%s", baseURL, apiPath)

	// Get the data
	httpClient := client.httpClient
	req, err := http.NewRequest("GET", url, nil)
	req.Header.Add("Content-Type", "application/json; charset=UTF-8")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Authorization", "Bearer "+client.sessionID)

	// resp, err := http.Get(url)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Create the file
	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	return err
}

func parseHost(input string) string {
	parsed, err := url.Parse(input)
	if err == nil {
		return fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
	}
	return "Failed to parse URL input"
}

//Get the List of all available objects and their metadata for your organization's data
func (client *Client) DescribeGlobal() (*SObjectMeta, error) {
	apiPath := fmt.Sprintf("/services/data/v%s/sobjects", client.apiVersion)
	baseURL := strings.TrimRight(client.baseURL, "/")
	url := fmt.Sprintf("%s%s", baseURL, apiPath) // Get the objects
	httpClient := client.httpClient
	req, err := http.NewRequest("GET", url, nil)
	req.Header.Add("Content-Type", "application/json; charset=UTF-8")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Authorization", "Bearer "+client.sessionID)
	// resp, err := http.Get(url)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var meta SObjectMeta

	respData, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println(logPrefix, "error while reading all body")
		return nil, fmt.Errorf(`{"error" : %w, "code": %d}`, err, http.StatusInternalServerError)
	}

	err = json.Unmarshal(respData, &meta)
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

//Get the list of all created and updated objects, name of the type od the object records and the list will be fetched as per between start date/time and end date/time
func (client *Client) GetCreatedUpdatedRecords(name, startDateTime, endDateTime string) ([]*SObject, error) {
	if !client.isLoggedIn() {
		return nil, ERR_AUTHENTICATION
	}
	formatString := "sobjects/%s/updated/?start=%s&end=%s"
	baseURL := client.makeURL(formatString)
	url := fmt.Sprintf(baseURL, name, url.QueryEscape(startDateTime), url.QueryEscape(endDateTime))
	httpClient := client.httpClient

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, ERR_FAILURE
	}
	req.Header.Add("Content-Type", "application/json; charset=UTF-8")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Authorization", "Bearer "+client.sessionID)
	// resp, err := http.Get(url)
	resp, err := httpClient.Do(req)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Println(logPrefix, "request failed,", resp.StatusCode)
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		theError := ParseSalesforceError(resp.StatusCode, buf.Bytes())
		return nil, theError
	}
	defer resp.Body.Close()

	if RetryLogic(resp.StatusCode) {
		return nil, ERR_RETRY
	}
	var (
		sobj  SObject
		sobjs []*SObject
		wg    sync.WaitGroup
	)

	respData, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println(logPrefix, "error while reading all body")
		return nil, ERR_FAILURE
	}

	err = json.Unmarshal(respData, &sobj)
	if err != nil {
		return nil, ERR_FAILURE
	}

	ids, ok := sobj["ids"]
	if !ok {
		return nil, ERR_FAILURE
	}

	sobjIds, ok := ids.([]interface{})
	if !ok {
		return nil, ERR_FAILURE
	}

	if len(sobjIds) > 0 {
		sobjs = make([]*SObject, len((sobjIds)))
		for i, id := range sobjIds {
			wg.Add(1)
			go func(i int, id string, wg *sync.WaitGroup) {
				sobj, err := client.SObject(name).Get(id)
				if err == nil {
					sobjs[i] = sobj
				}
				wg.Done()
			}(i, id.(string), &wg)
		}
		wg.Wait()
	}

	return sobjs, nil
}

func (client *Client) RefreshToken(clientId, clientSecret, refreshToken string) (interface{}, error) {
	formatString := "services/oauth2/token"
	baseURL := client.makeURL(formatString)
	httpClient := client.httpClient
	params := url.Values{
		"format":        {"json"},
		"grant_type":    {"refresh_token"},
		"client_id":     {clientId},
		"client_secret": {clientSecret},
		"refresh_token": {refreshToken},
	}

	req, err := http.NewRequest("POST", baseURL, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, ERR_FAILURE
	}
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Println(logPrefix, "request failed,", resp.StatusCode)
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		theError := ParseSalesforceError(resp.StatusCode, buf.Bytes())
		return nil, theError
	}
	defer resp.Body.Close()
	token := new(Token)
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if unmarshalErr := json.Unmarshal(data, token); unmarshalErr != nil {
		return nil, unmarshalErr
	}
	return token, nil
}

func (client *Client) RevokeToken(refreshToken string) error {
	formatString := "services/oauth2/revoke"
	baseURL := client.makeURL(formatString)
	httpClient := client.httpClient
	params := url.Values{
		"token": {refreshToken},
	}

	req, err := http.NewRequest("POST", baseURL, strings.NewReader(params.Encode()))
	if err != nil {
		return ERR_FAILURE
	}
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Println(logPrefix, "request failed,", resp.StatusCode)
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		theError := ParseSalesforceError(resp.StatusCode, buf.Bytes())
		return theError
	}

	return nil
}
