package config

import (
	"io"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

var required = []string{"-source-repo=s", "-target-repo=t", "-filter-dir=d"}

func TestLoadPrecedence(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want Config
	}{
		{
			name: "defaults",
			args: required,
			want: Config{WorkDir: "work", SourceRepo: "s", SourceBranch: "master",
				TargetRepo: "t", TargetBranch: "master", FilterDir: "d"},
		},
		{
			name: "env only",
			env: map[string]string{"SOURCE_REPO": "es", "TARGET_REPO": "et", "FILTER_DIR": "a/b",
				"SOURCE_BRANCH": "dev", "TARGET_BRANCH": "main", "WORKDIR": "/w"},
			want: Config{WorkDir: "/w", SourceRepo: "es", SourceBranch: "dev",
				TargetRepo: "et", TargetBranch: "main", FilterDir: "a/b"},
		},
		{
			// Regression: TargetRepo used to read SOURCE_REPO, and flags clobbered env.
			name: "flag beats env, env not cross-wired",
			args: []string{"-target-repo=ft"},
			env:  map[string]string{"SOURCE_REPO": "es", "TARGET_REPO": "et", "FILTER_DIR": "d"},
			want: Config{WorkDir: "work", SourceRepo: "es", SourceBranch: "master",
				TargetRepo: "ft", TargetBranch: "master", FilterDir: "d"},
		},
		{
			name: "filter dir normalized",
			args: []string{"-source-repo=s", "-target-repo=t", "-filter-dir=/lib//foo/"},
			want: Config{WorkDir: "work", SourceRepo: "s", SourceBranch: "master",
				TargetRepo: "t", TargetBranch: "master", FilterDir: "lib/foo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load(tt.args, envMap(tt.env), io.Discard)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if *got != tt.want {
				t.Errorf("got %+v\nwant %+v", *got, tt.want)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		wantErr string
	}{
		{"missing all", nil, nil, "-source-repo is required"},
		{"missing filter", []string{"-source-repo=s", "-target-repo=t"}, nil, "-filter-dir is required"},
		{"root filter", []string{"-source-repo=s", "-target-repo=t", "-filter-dir=/"}, nil, "must name a subdirectory"},
		{"escaping filter", []string{"-source-repo=s", "-target-repo=t", "-filter-dir=../x"}, nil, "must name a subdirectory"},
		{"stray arg", append(required, "extra"), nil, "unexpected arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.args, envMap(tt.env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
