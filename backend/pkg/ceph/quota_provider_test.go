package ceph

import "testing"

func TestStorageQuotaEnabledRequiresExplicitOptIn(t *testing.T) {
	t.Parallel()

	enabled := true
	disabled := false
	tests := []struct {
		name    string
		enabled *bool
		want    bool
	}{
		{name: "omitted", enabled: nil, want: false},
		{name: "disabled", enabled: &disabled, want: false},
		{name: "enabled", enabled: &enabled, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := storageQuotaEnabled(tt.enabled); got != tt.want {
				t.Fatalf("storageQuotaEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSetDirectoryQuotaRejectsAmbiguousValues(t *testing.T) {
	t.Parallel()

	for _, quota := range []int64{-2, 0} {
		if err := setDirectoryQuota(nil, nil, "", "users/alice", quota); err == nil {
			t.Errorf("setDirectoryQuota() accepted quota %d", quota)
		}
	}
}

func TestLogicalPathToStorageRelativePath(t *testing.T) {
	t.Parallel()

	prefixes := StoragePrefixConfig{User: "users", Account: "accounts", Public: "public"}
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{name: "user", path: "/user/alice", want: "users/alice"},
		{name: "nested user path", path: "/user/alice/checkpoints", want: "users/alice/checkpoints"},
		{name: "public root", path: "/public", want: "public"},
		{name: "account", path: "/account/lab", want: "accounts/lab"},
		{name: "missing user space", path: "/user", wantErr: true},
		{name: "escape prefix", path: "/user/../../public", wantErr: true},
		{name: "unknown type", path: "/other/alice", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := logicalPathToStorageRelativePath(tt.path, prefixes)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("path = %q, want %q", got, tt.want)
			}
		})
	}
}
