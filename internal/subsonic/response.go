package subsonic

import (
	"encoding/json"
	"encoding/xml"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

const (
	contentTypeXML  = "application/xml; charset=utf-8"
	contentTypeJSON = "application/json; charset=utf-8"
)

// WriteOK writes an HTTP 200 OK OpenSubsonic success envelope in XML (default)
// or JSON (f=json). It always populates envelope metadata (Xmlns, Status="ok",
// Version="1.16.1", Type="headunit", ServerVersion="0.1.0", OpenSubsonic=true)
// and clears resp.Error.
func WriteOK(w http.ResponseWriter, r *http.Request, resp Response) {
	writeOKWithLogger(w, r, nil, resp)
}

// WriteError writes an HTTP 200 OK OpenSubsonic error envelope (status="failed")
// with the given Subsonic application error code and message. Signature matches
// auth.ErrorWriter.
func WriteError(w http.ResponseWriter, r *http.Request, code int, message string) {
	writeErrorWithLogger(w, r, nil, code, message)
}

func writeOKWithLogger(w http.ResponseWriter, r *http.Request, logger *slog.Logger, resp Response) {
	resp.Xmlns = xmlNamespace
	resp.Status = statusOK
	resp.Version = apiVersion
	resp.Type = serverType
	resp.ServerVersion = serverVersion
	resp.OpenSubsonic = true
	resp.Error = nil

	writeResponse(w, r, logger, resp)
}

func writeErrorWithLogger(w http.ResponseWriter, r *http.Request, logger *slog.Logger, code int, message string) {
	resp := Response{
		Xmlns:         xmlNamespace,
		Status:        statusFailed,
		Version:       apiVersion,
		Type:          serverType,
		ServerVersion: serverVersion,
		OpenSubsonic:  true,
		Error: &APIError{
			Code:    code,
			Message: message,
		},
	}

	writeResponse(w, r, logger, resp)
}

func writeResponse(w http.ResponseWriter, r *http.Request, logger *slog.Logger, resp Response) {
	isJSON := wantsJSON(r)

	var (
		payload     []byte
		contentType string
		err         error
	)

	if isJSON {
		contentType = contentTypeJSON
		payload, err = json.Marshal(jsonEnvelope{SubsonicResponse: resp})
	} else {
		contentType = contentTypeXML
		var body []byte
		body, err = xml.Marshal(resp)
		if err == nil {
			payload = make([]byte, 0, len(xml.Header)+len(body))
			payload = append(payload, xml.Header...)
			payload = append(payload, body...)
		}
	}

	if err != nil {
		if logger != nil {
			format := "xml"
			if isJSON {
				format = "json"
			}
			logger.Error("subsonic: failed to marshal response", "format", format, "error", err)
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(http.StatusOK)

	if r != nil && r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(payload)
}

// wantsJSON reports whether r requests JSON output via f=json without invoking
// r.FormValue (which would trigger ParseMultipartForm on unbounded bodies).
func wantsJSON(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(formParam(r, "f")), "json")
}

// formParam reads a single request parameter from pre-parsed r.Form or, when
// r.Form has not been populated, from the URL query string without calling
// r.FormValue or ParseMultipartForm.
func formParam(r *http.Request, key string) string {
	if r == nil {
		return ""
	}
	if r.Form != nil {
		return r.Form.Get(key)
	}
	if r.URL != nil {
		return r.URL.Query().Get(key)
	}
	return ""
}
