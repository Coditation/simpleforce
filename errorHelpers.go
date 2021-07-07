package simpleforce

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"log"
)

type SfdcError struct {
	Code    string
	Message string
	Extra   interface{}
}

func (se SfdcError) Error() string {
	return se.Message
}

var (
	// ErrFailure is a generic error if none of the other errors are appropriate.
	ERR_FAILURE = SfdcError{Message: "general failure", Code: "GENERAL_FAILURE"}

	// ErrAuthentication is returned when authentication failed.
	ERR_AUTHENTICATION = SfdcError{Message: "authentication failure", Code: "AUTH_ERROR"}

	//ERR_DATA_NOT_FOUND is returned when data is not found
	ERR_DATA_NOT_FOUND = SfdcError{Message: "data not found", Code: "NOT_FOUND"}

	//Error codes implements the retry logic
	errorCodes = []int{500, 503, 403}

	//ERR_RETRY to implement backoff
	ERR_RETRY = errors.New("retry call")
)

type jsonError []struct {
	Message   string `json:"message"`
	ErrorCode string `json:"errorCode"`
}

type xmlError struct {
	Message   string `xml:"Body>Fault>faultstring"`
	ErrorCode string `xml:"Body>Fault>faultcode"`
}

//Need to get information out of this package.
func ParseSalesforceError(statusCode int, responseBody []byte) (err error) {
	jsonError := jsonError{}
	xmlError := xmlError{}
	err = json.Unmarshal(responseBody, &jsonError)
	if err != nil {
		//Unable to parse json. Try xml
		err = xml.Unmarshal(responseBody, &xmlError)
		if err != nil {
			//Unable to parse json or XML
			log.Println("ERROR UNMARSHALLING: ", err)
			return ERR_FAILURE
		}
		//successfully parsed XML:
		err = SfdcError{Message: xmlError.Message, Code: xmlError.ErrorCode, Extra: map[string]interface{}{"StatusCode": statusCode}}
		return err
	} else {
		//Successfully parsed json error:
		err = SfdcError{Message: jsonError[0].Message, Code: jsonError[0].ErrorCode, Extra: map[string]interface{}{"StatusCode": statusCode}}
		return err
	}
}

func RetryLogic(n int) bool {
	for i := range errorCodes {
		if errorCodes[i] == n {
			return true
		}
	}
	return false
}
