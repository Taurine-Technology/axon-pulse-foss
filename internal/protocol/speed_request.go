package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

const (
	speedRequestSignatureVersion = "axon-pulse-speed-test-v1"
)

// SignSpeedTestRequest binds a one-off command to this sensor's current
// enrollment secret. Rotation of that secret makes old directives invalid.
func SignSpeedTestRequest(request SpeedTestRequest, credentials Credentials) (string, error) {
	if credentials.SensorID == "" {
		return "", errors.New("missing sensor ID")
	}
	secret, err := decodeSensorSecret(credentials.Secret)
	if err != nil {
		return "", err
	}
	canonical := strings.Join([]string{
		speedRequestSignatureVersion,
		credentials.SensorID,
		request.Nonce,
		request.Profile,
		strconv.FormatInt(request.IssuedAt, 10),
		strconv.FormatInt(request.ExpiresAt, 10),
	}, "\n")
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(canonical))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

func VerifySpeedTestRequest(request SpeedTestRequest, credentials Credentials) error {
	expected, err := SignSpeedTestRequest(request, credentials)
	if err != nil {
		return err
	}
	provided, err := base64.StdEncoding.DecodeString(request.Signature)
	if err != nil {
		return errors.New("invalid speed-test request signature")
	}
	want, _ := base64.StdEncoding.DecodeString(expected)
	if !hmac.Equal(provided, want) {
		return errors.New("invalid speed-test request signature")
	}
	return nil
}
