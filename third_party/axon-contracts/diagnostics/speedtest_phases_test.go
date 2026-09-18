package diagnostics

import (
	"context"
	"strings"
	"testing"
	"time"
)

type (
	adversarialSpeedEndpoint struct {
		host   string
		stream func(context.Context, uint64, time.Time, func(int) bool) error
		upload func(context.Context, uint64, time.Time) (uint64, time.Duration, error)
	}
)

func (e *adversarialSpeedEndpoint) Host() string { return e.host }

func (e *adversarialSpeedEndpoint) Download(
	context.Context,
	uint64,
	time.Time,
) (uint64, time.Duration, time.Duration, error) {
	return 0, 0, 0, nil
}

func (e *adversarialSpeedEndpoint) StreamDownload(
	ctx context.Context,
	nbytes uint64,
	deadline time.Time,
	onRead func(int) bool,
) error {
	return e.stream(ctx, nbytes, deadline, onRead)
}

func (e *adversarialSpeedEndpoint) Upload(
	ctx context.Context,
	nbytes uint64,
	deadline time.Time,
) (uint64, time.Duration, error) {
	return e.upload(ctx, nbytes, deadline)
}

func (e *adversarialSpeedEndpoint) Close() error { return nil }

func TestRunBidirectionalPhaseRecoversDownloadPanic(t *testing.T) {
	downloadEndpoint := &adversarialSpeedEndpoint{
		host: "download",
		stream: func(context.Context, uint64, time.Time, func(int) bool) error {
			panic("stream exploded")
		},
		upload: successfulAdversarialUpload,
	}
	uploadEndpoint := &adversarialSpeedEndpoint{
		host: "upload",
		stream: func(context.Context, uint64, time.Time, func(int) bool) error {
			return nil
		},
		upload: successfulAdversarialUpload,
	}

	download, upload, _ := runBidirectionalPhase(
		context.Background(),
		bidirectionalTestSpec(downloadEndpoint, uploadEndpoint, time.Now().Add(time.Second)),
	)

	if !strings.Contains(download.Error, "bidirectional download raised: stream exploded") {
		t.Fatalf("download panic result = %+v", download)
	}
	if upload.Error != "" || upload.BytesTransferred == 0 {
		t.Fatalf("upload should still complete: %+v", upload)
	}
}

func TestRunBidirectionalPhaseBoundsNonReturningDownload(t *testing.T) {
	release := make(chan struct{})
	downloadEndpoint := &adversarialSpeedEndpoint{
		host: "download",
		stream: func(context.Context, uint64, time.Time, func(int) bool) error {
			<-release
			return nil
		},
		upload: successfulAdversarialUpload,
	}
	uploadEndpoint := &adversarialSpeedEndpoint{
		host: "upload",
		stream: func(context.Context, uint64, time.Time, func(int) bool) error {
			return nil
		},
		upload: successfulAdversarialUpload,
	}

	started := time.Now()
	download, _, _ := runBidirectionalPhase(
		context.Background(),
		bidirectionalTestSpec(downloadEndpoint, uploadEndpoint, time.Now().Add(30*time.Millisecond)),
	)
	close(release)

	// The phase may spend the post-deadline drain grace on a worker that
	// ignores cancellation, but never more than deadline + grace + slack.
	if elapsed := time.Since(started); elapsed > bidirectionalDrainGrace+300*time.Millisecond {
		t.Fatalf("bidirectional phase blocked beyond deadline: %v", elapsed)
	}
	if !download.CapHit || !strings.Contains(download.Error, "did not stop before deadline") {
		t.Fatalf("non-returning download result = %+v", download)
	}
}

func TestRunBidirectionalPhaseDrainsPartialResultsAfterDeadline(t *testing.T) {
	// A deadline-aware worker reports the bytes it actually moved shortly
	// after cancellation; that real partial measurement must survive
	// instead of being replaced by a zero-byte synthetic truncation.
	downloadEndpoint := &adversarialSpeedEndpoint{
		host: "download",
		stream: func(ctx context.Context, _ uint64, _ time.Time, onRead func(int) bool) error {
			onRead(512)
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond)
			return ctx.Err()
		},
		upload: successfulAdversarialUpload,
	}
	uploadEndpoint := &adversarialSpeedEndpoint{
		host: "upload",
		stream: func(context.Context, uint64, time.Time, func(int) bool) error {
			return nil
		},
		upload: func(ctx context.Context, nbytes uint64, _ time.Time) (uint64, time.Duration, error) {
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond)
			return nbytes / 2, 20 * time.Millisecond, ctx.Err()
		},
	}

	download, upload, _ := runBidirectionalPhase(
		context.Background(),
		bidirectionalTestSpec(downloadEndpoint, uploadEndpoint, time.Now().Add(30*time.Millisecond)),
	)

	if download.BytesTransferred == 0 {
		t.Fatalf("drained download lost its partial bytes: %+v", download)
	}
	if upload.BytesTransferred == 0 {
		t.Fatalf("drained upload lost its partial bytes: %+v", upload)
	}
}

func bidirectionalTestSpec(
	downloadEndpoint SpeedEndpoint,
	uploadEndpoint SpeedEndpoint,
	deadline time.Time,
) bidirectionalPhaseSpec {
	return bidirectionalPhaseSpec{
		downloadURL:    "download",
		uploadURL:      "upload",
		downloadBudget: 1024,
		uploadBudget:   1024,
		deadline:       deadline,
		endpointFactory: func(rawURL string) (SpeedEndpoint, error) {
			if rawURL == "download" {
				return downloadEndpoint, nil
			}
			return uploadEndpoint, nil
		},
		skipLatency: true,
	}
}

func successfulAdversarialUpload(
	ctx context.Context,
	nbytes uint64,
	deadline time.Time,
) (uint64, time.Duration, error) {
	_ = ctx
	_ = deadline
	return nbytes, time.Millisecond, nil
}
