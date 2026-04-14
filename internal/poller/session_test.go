package poller

import (
	"testing"

	"github.com/gosnmp/gosnmp"
)

func TestMapAuthProto(t *testing.T) {
	tests := []struct {
		input string
		want  gosnmp.SnmpV3AuthProtocol
	}{
		{"md5", gosnmp.MD5},
		{"MD5", gosnmp.MD5},
		{"sha", gosnmp.SHA},
		{"SHA", gosnmp.SHA},
		{"sha1", gosnmp.SHA},
		{"SHA1", gosnmp.SHA},
		{"sha128", gosnmp.SHA},
		{"SHA128", gosnmp.SHA},
		{"sha224", gosnmp.SHA224},
		{"sha256", gosnmp.SHA256},
		{"sha384", gosnmp.SHA384},
		{"sha512", gosnmp.SHA512},
		{"noauth", gosnmp.NoAuth},
		{"", gosnmp.NoAuth},
		{"unknown", gosnmp.NoAuth},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mapAuthProto(tt.input)
			if got != tt.want {
				t.Errorf("mapAuthProto(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestMapPrivProto(t *testing.T) {
	tests := []struct {
		input string
		want  gosnmp.SnmpV3PrivProtocol
	}{
		{"des", gosnmp.DES},
		{"DES", gosnmp.DES},
		{"des56", gosnmp.DES},
		{"DES56", gosnmp.DES},
		{"aes", gosnmp.AES},
		{"AES", gosnmp.AES},
		{"aes128", gosnmp.AES},
		{"AES128", gosnmp.AES},
		{"aes192", gosnmp.AES192},
		{"aes256", gosnmp.AES256},
		{"aes192c", gosnmp.AES192C},
		{"aes256c", gosnmp.AES256C},
		{"nopriv", gosnmp.NoPriv},
		{"", gosnmp.NoPriv},
		{"unknown", gosnmp.NoPriv},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mapPrivProto(tt.input)
			if got != tt.want {
				t.Errorf("mapPrivProto(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
