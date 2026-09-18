package protocol

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestSpeedTestRequestSignatureBindsEnrollmentAndFields(t *testing.T) {
	t.Parallel()
	credentials := Credentials{SensorID: "sensor-a", Secret: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))}
	now := time.Now()
	request := SpeedTestRequest{Nonce: "nonce_1234567890", Profile: "content", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}
	signature, err := SignSpeedTestRequest(request, credentials)
	if err != nil {
		t.Fatal(err)
	}
	request.Signature = signature
	if err := VerifySpeedTestRequest(request, credentials); err != nil {
		t.Fatal(err)
	}
	request.Profile = "capacity"
	if err := VerifySpeedTestRequest(request, credentials); err == nil {
		t.Fatal("modified request retained a valid signature")
	}
	request.Profile = "content"
	if err := VerifySpeedTestRequest(request, Credentials{SensorID: "sensor-b", Secret: credentials.Secret}); err == nil {
		t.Fatal("request signature was not bound to the enrollment identity")
	}
}
