package drive

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

func TestRetryable(t *testing.T) {
	rate := &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}}
	full := &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "storageQuotaExceeded"}}}
	tests := []struct {
		err  error
		want bool
	}{
		{&googleapi.Error{Code: 429}, true},
		{&googleapi.Error{Code: 503}, true},
		{fmt.Errorf("wrapped: %w", rate), true},
		{&googleapi.Error{Code: 404}, false},
		{full, false},
		{&url.Error{Op: "Post", Err: fmt.Errorf("connection reset")}, true},
		{&url.Error{Op: "Post", Err: &oauth2.RetrieveError{ErrorCode: "invalid_grant"}}, false},
		{&os.PathError{Op: "open", Err: os.ErrPermission}, false},
		{context.Canceled, false},
		{ErrTransient, true},
		{fmt.Errorf("upload: %w", ErrOffline), true},
	}
	for _, tt := range tests {
		if got := Retryable(tt.err); got != tt.want {
			t.Errorf("Retryable(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
	for err, want := range map[error]bool{
		&url.Error{Op: "Post", Err: fmt.Errorf("dial tcp: no route to host")}:          true,
		fmt.Errorf("upload: %w", ErrOffline):                                           true,
		&url.Error{Op: "Post", Err: &oauth2.RetrieveError{ErrorCode: "invalid_grant"}}: false,
		&googleapi.Error{Code: 503}:                                                    false,
		ErrTransient:                                                                   false,
		context.Canceled:                                                               false,
	} {
		if got := IsNetwork(err); got != want {
			t.Errorf("IsNetwork(%v) = %v, want %v", err, got, want)
		}
	}
	if !IsStorageFull(fmt.Errorf("upload: %w", full)) || IsStorageFull(rate) {
		t.Error("IsStorageFull")
	}
}

func TestQuote(t *testing.T) {
	if got := quote(`it's a\b`); got != `'it\'s a\\b'` {
		t.Fatalf("quote = %s", got)
	}
}
