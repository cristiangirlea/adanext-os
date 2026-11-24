package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

// ListenerStatus is the network listener's setting and state as the owner sees it.
type ListenerStatus struct {
	Enabled     bool                 `json:"enabled"`
	Address     string               `json:"address"`
	Name        string               `json:"name"`
	Origin      string               `json:"origin"`
	State       string               `json:"state"`  // serving or off
	Reason      string               `json:"reason"` // "" while serving, else why it is off
	Message     string               `json:"message"`
	Certificate *ListenerCertificate `json:"certificate"` // nil when no pair is installed
}

// ListenerCertificate describes the installed pair's leaf, valid or not.
type ListenerCertificate struct {
	Names     []string `json:"names"`
	NotBefore string   `json:"not_before"`
	NotAfter  string   `json:"not_after"`
	SHA256    string   `json:"sha256"`
}

// ListenerSetting is the owner's listener setting; "" leaves the address or the name unset.
type ListenerSetting struct {
	Enabled       bool
	Address, Name string
}

// ListenerControl is the coordinator's network listener, driven from the owner's channel.
type ListenerControl interface {
	Status() ListenerStatus
	Configure(ctx context.Context, s ListenerSetting) (ListenerStatus, error)
	InstallCertificate(ctx context.Context, certPath, keyPath string) (ListenerStatus, error)
}

// ListenerError is a refused setting or certificate: Code is the API's error code and Message a
// sentence for the owner, which never holds key material.
type ListenerError struct {
	Code, Message string
}

func (e *ListenerError) Error() string { return e.Code + ": " + e.Message }

// ErrListenerStopped is a change refused because the coordinator is stopping.
var ErrListenerStopped = errors.New("the network listener has stopped")

// listenerRefusals are the codes a ListenerError answers with 400.
var listenerRefusals = map[string]bool{"address_invalid": true, "name_invalid": true, "certificate_invalid": true, "certificate_expired": true, "certificate_name": true}

// ValidateListenerSetting refuses an address that is not a usable IP literal and port, and a
// name that is not a lowercase DNS host name; either may be "" while unset.
func ValidateListenerSetting(s ListenerSetting) error {
	if s.Address != "" && !validListenerAddress(s.Address) {
		return &ListenerError{"address_invalid", "The address must be an IP address of this host and a port from 1 to 65535, such as 192.168.1.20:8443; 0.0.0.0, :: and multicast addresses are refused."}
	}
	if s.Name != "" && !validListenerName(s.Name) {
		return &ListenerError{"name_invalid", "The name must be a lowercase DNS host name with at least one dot, such as home.adanext.si, or localhost."}
	}
	return nil
}

func validListenerAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(host)
	// One interface address needs no zone, and a zone hides an unspecified address from the checks.
	if err != nil || addr.Zone() != "" || addr.Unmap().IsUnspecified() || addr.Unmap().IsMulticast() {
		return false
	}
	return validPort(port)
}

func validPort(port string) bool {
	// No leading zero, so the origin built from the port is canonical.
	if port == "" || len(port) > 5 || port[0] == '0' || strings.Trim(port, "0123456789") != "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

func validListenerName(name string) bool {
	if len(name) > 253 {
		return false
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return false
	}
	if name != "localhost" && !strings.Contains(name, ".") {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		if strings.Trim(label, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			return false
		}
	}
	return true
}

// ListenerOrigin is https://name, with :port unless the port is 443; "" while either is unset.
func ListenerOrigin(name, address string) string {
	if name == "" || address == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return ""
	}
	if port == "443" {
		return "https://" + name
	}
	return "https://" + name + ":" + port
}

// listenerFailure answers a refusal with its code and message, anything else as internal.
func listenerFailure(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrListenerStopped) {
		writeError(w, 503, "shutting_down")
		return
	}
	if e, ok := errors.AsType[*ListenerError](err); ok && listenerRefusals[e.Code] {
		writeJSON(w, 400, errorBody{e.Code, e.Message})
		return
	}
	writeInternalError(w, err)
}

// listener is the owner's control of the network listener, or false once it has answered.
func (h *handler) listener(w http.ResponseWriter, r *http.Request) (ListenerControl, bool) {
	if _, ok := h.ownerActor(w, r); !ok {
		return nil, false
	}
	if h.exec.Listener == nil {
		writeError(w, 503, "listener_unavailable")
		return nil, false
	}
	return h.exec.Listener, true
}

func (h *handler) getListener(w http.ResponseWriter, r *http.Request) {
	l, ok := h.listener(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, l.Status())
}

func (h *handler) setListener(w http.ResponseWriter, r *http.Request) {
	l, ok := h.listener(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var input struct {
		Enabled *bool   `json:"enabled"`
		Address *string `json:"address"`
		Name    *string `json:"name"`
	}
	if err := decode(body, &input); err != nil || input.Enabled == nil || input.Address == nil || input.Name == nil {
		writeError(w, 400, "bad_request")
		return
	}
	status, err := l.Configure(r.Context(), ListenerSetting{Enabled: *input.Enabled, Address: *input.Address, Name: *input.Name})
	if err != nil {
		listenerFailure(w, err)
		return
	}
	writeJSON(w, 200, status)
}

func (h *handler) installCertificate(w http.ResponseWriter, r *http.Request) {
	l, ok := h.listener(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var input struct {
		Certificate string `json:"certificate"`
		Key         string `json:"key"`
	}
	if err := decode(body, &input); err != nil || !validRepoPath(input.Certificate) || !validRepoPath(input.Key) {
		writeError(w, 400, "bad_request")
		return
	}
	status, err := l.InstallCertificate(r.Context(), input.Certificate, input.Key)
	if err != nil {
		listenerFailure(w, err)
		return
	}
	writeJSON(w, 200, status)
}
