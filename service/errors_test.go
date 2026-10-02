package service

import (
	"fmt"
	"testing"

	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

func TestClassifySentinels(t *testing.T) {
	cases := []struct {
		err  error
		want ErrorClass
	}{
		{nil, ""},
		{ErrRepoNotFound, ErrorClassNotFound},
		{ErrTaskNotFound, ErrorClassNotFound},
		{ErrRuleNotFound, ErrorClassNotFound},
		{ErrEventNotFound, ErrorClassNotFound},
		{ErrPlatformNotFound, ErrorClassNotFound},
		{ErrTargetPlatformNotFound, ErrorClassNotFound},
		{fmt.Errorf("update task: %w", ErrTaskNotFound), ErrorClassNotFound},
		{ErrTaskRunning, ErrorClassConflict},
		{ErrTooManyConcurrent, ErrorClassConflict},
		{ErrTaskDisabled, ErrorClassValidation},
		{errBackupDisabled, ErrorClassValidation},
		{errInvalidName, ErrorClassValidation},
		{errLegalHold, ErrorClassConflict},
		{newMetadataError("sync.backup_dir not configured", nil), ErrorClassValidation},
		{fmt.Errorf("wrapped: %w", newMetadataError("no snapshot for r1", nil)), ErrorClassValidation},
		{fmt.Errorf("plain"), ErrorClassInternal},
	}
	for i, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Fatalf("case %d (%v): got %q want %q", i, c.err, got, c.want)
		}
	}
}

func TestClassifyProviderErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"404", &sdkprov.ProviderError{Op: "GetRepo", StatusCode: 404, Cause: sdkprov.ErrNotFound}, ErrorClassNotFound},
		{"401", &sdkprov.ProviderError{Op: "ListRepos", StatusCode: 401, Cause: sdkprov.ErrAuthentication}, ErrorClassAuth},
		{"429", &sdkprov.ProviderError{Op: "ListRepos", StatusCode: 429, Cause: sdkprov.ErrRateLimited}, ErrorClassRateLimited},
		{"500", &sdkprov.ProviderError{Op: "ListRepos", StatusCode: 500}, ErrorClassUnavailable},
		{"400", &sdkprov.ProviderError{Op: "ListRepos", StatusCode: 400}, ErrorClassValidation},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wrapped := fmt.Errorf("list repos: %w", c.err)
			if got := Classify(wrapped); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}
