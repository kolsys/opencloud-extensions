// Package httpx holds the HTTP pieces both extensions share: errors in the
// format of the platform, a transparent reverse proxy to its services, and
// the PROPFIND that authorises a request.
package httpx

import (
	"encoding/xml"
	"net/http"
	"strconv"
)

// Namespaces of a DAV error, copied from the platform verbatim. The d one is
// "DAV" and not "DAV:" there, and clients are matched against it.
const (
	namespaceDAV     = "DAV"
	namespaceSabre   = "http://sabredav.org/ns"
	contentTypeXML   = "application/xml; charset=utf-8"
	exceptionUnknown = ""
)

// exceptions are the ones the platform names. It leaves the element empty for
// every other status, 425 and 429 among them.
var exceptions = map[int]string{
	http.StatusBadRequest:       `Sabre\DAV\Exception\BadRequest`,
	http.StatusUnauthorized:     `Sabre\DAV\Exception\NotAuthenticated`,
	http.StatusNotFound:         `Sabre\DAV\Exception\NotFound`,
	http.StatusMethodNotAllowed: `Sabre\DAV\Exception\MethodNotAllowed`,
}

// DAVError is the error body of the platform.
type DAVError struct {
	XMLName   xml.Name `xml:"d:error"`
	Xmlnsd    string   `xml:"xmlns:d,attr"`
	Xmlnss    string   `xml:"xmlns:s,attr"`
	Exception string   `xml:"s:exception"`
	Message   string   `xml:"s:message"`
}

// NewDAVError builds the body the platform would send for a status.
func NewDAVError(status int, message string) *DAVError {
	exception, ok := exceptions[status]
	if !ok {
		exception = exceptionUnknown
	}

	return &DAVError{
		Xmlnsd:    namespaceDAV,
		Xmlnss:    namespaceSabre,
		Exception: exception,
		Message:   message,
	}
}

// WriteDAVError answers in the format of the platform, so that the web treats
// the answer of an extension like one of its own services.
func WriteDAVError(w http.ResponseWriter, status int, message string) {
	body, err := xml.Marshal(NewDAVError(status, message))
	if err != nil {
		http.Error(w, http.StatusText(status), status)
		return
	}

	w.Header().Set("Content-Type", contentTypeXML)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

// NotFoundMessage is the wording the platform uses for a missing file.
func NotFoundMessage(name string) string {
	return "File with name " + name + " could not be located"
}

// WriteNotFound answers 404 the way the platform does. A thumbnail that
// failed for good is reported this way, so that the web stops asking.
func WriteNotFound(w http.ResponseWriter, name string) {
	WriteDAVError(w, http.StatusNotFound, NotFoundMessage(name))
}

// WriteTooEarly answers 425 while the thumbnail is being generated.
//
// The Retry-After is an addition of ours: the platform sets that header on
// 429 only, and a client has nothing else to pace itself with.
func WriteTooEarly(w http.ResponseWriter, retryAfter int) {
	if retryAfter < 1 {
		retryAfter = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	WriteDAVError(w, http.StatusTooEarly, "")
}
