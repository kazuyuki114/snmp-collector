// Package poller implements the SNMP polling stage of the pipeline.
// It converts device configuration into live gosnmp sessions, manages a
// per-device connection pool, and executes Get / BulkWalk operations that
// produce RawPollResult messages consumed by the decoder stage.
package poller

import (
	"fmt"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
	"snmp/snmp-collector/pkg/snmpcollector/config"
)

// ─────────────────────────────────────────────────────────────────────────────
// Session factory — DeviceConfig → *gosnmp.GoSNMP
// ─────────────────────────────────────────────────────────────────────────────

// NewSession creates and connects a gosnmp session for the given device
// configuration. The caller is responsible for calling Close when the session
// is no longer needed.
func NewSession(cfg config.DeviceConfig) (*gosnmp.GoSNMP, error) {
	g := &gosnmp.GoSNMP{
		Target:             cfg.IP,
		Port:               uint16(cfg.Port),
		Timeout:            time.Duration(cfg.Timeout) * time.Millisecond,
		Retries:            cfg.Retries,
		ExponentialTimeout: cfg.ExponentialTimeout,
		MaxOids:            60,
	}

	switch cfg.Version {
	case "1":
		g.Version = gosnmp.Version1
		if len(cfg.Communities) > 0 {
			g.Community = cfg.Communities[0]
		}
	case "2c":
		g.Version = gosnmp.Version2c
		if len(cfg.Communities) > 0 {
			g.Community = cfg.Communities[0]
		}
	case "3":
		g.Version = gosnmp.Version3
		g.SecurityModel = gosnmp.UserSecurityModel
		if len(cfg.V3Credentials) > 0 {
			cred := cfg.V3Credentials[0]
			g.MsgFlags = snmpv3MsgFlags(cred)
			g.SecurityParameters = &gosnmp.UsmSecurityParameters{
				UserName:                 cred.Username,
				AuthenticationProtocol:   mapAuthProto(cred.AuthenticationProtocol),
				AuthenticationPassphrase: cred.AuthenticationPassphrase,
				PrivacyProtocol:          mapPrivProto(cred.PrivacyProtocol),
				PrivacyPassphrase:        cred.PrivacyPassphrase,
			}
		}
	default:
		return nil, fmt.Errorf("unsupported SNMP version %q", cfg.Version)
	}

	if err := g.Connect(); err != nil {
		return nil, fmt.Errorf("snmp connect %s:%d: %w", cfg.IP, cfg.Port, err)
	}
	return g, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// SNMPv3 helpers
// ─────────────────────────────────────────────────────────────────────────────

func snmpv3MsgFlags(cred config.V3Credentials) gosnmp.SnmpV3MsgFlags {
	hasAuth := cred.AuthenticationProtocol != "" &&
		!strings.EqualFold(cred.AuthenticationProtocol, "noauth")
	hasPriv := cred.PrivacyProtocol != "" &&
		!strings.EqualFold(cred.PrivacyProtocol, "nopriv")

	switch {
	case hasAuth && hasPriv:
		return gosnmp.AuthPriv
	case hasAuth:
		return gosnmp.AuthNoPriv
	default:
		return gosnmp.NoAuthNoPriv
	}
}

func mapAuthProto(s string) gosnmp.SnmpV3AuthProtocol {
	switch strings.ToLower(s) {
	case "md5":
		return gosnmp.MD5
	case "sha", "sha1", "sha128":
		return gosnmp.SHA
	case "sha224":
		return gosnmp.SHA224
	case "sha256":
		return gosnmp.SHA256
	case "sha384":
		return gosnmp.SHA384
	case "sha512":
		return gosnmp.SHA512
	default:
		return gosnmp.NoAuth
	}
}

func mapPrivProto(s string) gosnmp.SnmpV3PrivProtocol {
	switch strings.ToLower(s) {
	case "des", "des56":
		return gosnmp.DES
	case "aes", "aes128":
		return gosnmp.AES
	case "aes192":
		return gosnmp.AES192
	case "aes256":
		return gosnmp.AES256
	case "aes192c":
		return gosnmp.AES192C
	case "aes256c":
		return gosnmp.AES256C
	default:
		return gosnmp.NoPriv
	}
}
