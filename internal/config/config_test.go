package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsAndStorage(t *testing.T) {
	t.Setenv("DRIVE_DATABASE_URL", "postgres://example")
	t.Setenv("DRIVE_STORAGE_MODE", "local")
	c, e := Load()
	if e != nil || c.UploadMax != 20<<30 || c.ConvertMax != 100<<20 || c.AllowUnverifiedOCIS {
		t.Fatalf("unsafe config: %+v %v", c, e)
	}
	t.Setenv("DRIVE_STORAGE_MODE", "magic")
	if _, e = Load(); e == nil {
		t.Fatal("invalid storage allowed")
	}
}
func TestSecretFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(p, []byte(" secret\n"), 0600)
	t.Setenv("TEST_SECRET_FILE", p)
	v, e := Secret("TEST_SECRET")
	if e != nil || v != "secret" {
		t.Fatal(v, e)
	}
}

func baseConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DRIVE_DATABASE_URL", "postgres://example")
	t.Setenv("DRIVE_STORAGE_MODE", "local")
	for _, k := range []string{"DRIVE_MASTER_KEYS", "DRIVE_MASTER_KEYS_FILE", "DRIVE_MASTER_KEY_DEFAULT_ID", "DRIVE_MASTER_KEY_ALLOWED_CIDRS", "DRIVE_MASTER_KEY_STATUS_URL", "DRIVE_MASTER_KEY_STATUS_TOKEN", "DRIVE_MASTER_KEY_STATUS_TTL_SECONDS", "DRIVE_MASTER_KEY_SCOPES", "DRIVE_MASTER_KEY_INTERACTIVE_CLIENT_ID", "DRIVE_JOB_CREDENTIAL_MODE", "DRIVE_OCIS_SERVICE_TOKEN_URL", "DRIVE_OCIS_SERVICE_CLIENT_ID", "DRIVE_OCIS_SERVICE_CLIENT_SECRET", "DRIVE_OCIS_SERVICE_CLIENT_SECRET_FILE", "DRIVE_OCIS_SERVICE_AUDIENCE", "DRIVE_OCIS_SERVICE_SCOPES", "DRIVE_JOB_CREDENTIAL_TTL_SECONDS", "DRIVE_EXTERNAL_WRITE_ENABLED", "DRIVE_EXTERNAL_WRITE_ISSUERS", "DRIVE_OCIS_WRITE_SERVICE_TOKEN_URL", "DRIVE_OCIS_WRITE_SERVICE_CLIENT_ID", "DRIVE_OCIS_WRITE_SERVICE_CLIENT_SECRET", "DRIVE_OCIS_WRITE_SERVICE_CLIENT_SECRET_FILE", "DRIVE_OCIS_WRITE_SERVICE_AUDIENCE", "DRIVE_OCIS_WRITE_SERVICE_SCOPES", "DRIVE_OCIS_WRITE_SERVICE_TTL_SECONDS", "DRIVE_OCIS_WRITE_SERVICE_VERIFIED", "DRIVE_EXTERNAL_SPACE_ISSUERS", "DRIVE_SPACE_PROVISIONER_ENABLED", "DRIVE_SPACE_PROVISIONER_TOKEN_URL", "DRIVE_SPACE_PROVISIONER_CLIENT_ID", "DRIVE_SPACE_PROVISIONER_CLIENT_SECRET", "DRIVE_SPACE_PROVISIONER_CLIENT_SECRET_FILE", "DRIVE_SPACE_PROVISIONER_AUDIENCE", "DRIVE_SPACE_PROVISIONER_SCOPES", "DRIVE_SPACE_PROVISIONER_VERIFIED", "DRIVE_SPACE_PROVISIONER_NATIVE_INSTANCE"} {
		t.Setenv(k, "")
	}
}

func TestMasterKeyConfig(t *testing.T) {
	baseConfigEnv(t)
	t.Setenv("DRIVE_MASTER_KEYS", `{"k1":"secret1","k2":"secret2"}`)
	t.Setenv("DRIVE_MASTER_KEY_DEFAULT_ID", "k1")
	t.Setenv("DRIVE_MASTER_KEY_ALLOWED_CIDRS", "10.0.0.0/8, 127.0.0.1/32")
	t.Setenv("DRIVE_MASTER_KEY_STATUS_URL", "http://fenix-agent:3000/internal/drive/users/")
	t.Setenv("DRIVE_MASTER_KEY_STATUS_TTL_SECONDS", "30")
	t.Setenv("DRIVE_MASTER_KEY_SCOPES", "drive.read,drive.feedback")
	c, e := Load()
	if e != nil {
		t.Fatal(e)
	}
	if len(c.MasterKeys) != 2 || c.MasterKeys["k2"] != "secret2" || c.MasterKeyDefaultID != "k1" {
		t.Fatalf("keys not parsed: %+v", c.MasterKeys)
	}
	if c.MasterKeyIssuer != "fenix" || c.MasterKeyInteractiveClientID != c.WebClientID {
		t.Fatalf("defaults: %+v", c)
	}
	if len(c.MasterKeyAllowedCIDRs) != 2 || c.MasterKeyAllowedCIDRs[1] != "127.0.0.1/32" {
		t.Fatalf("cidrs: %+v", c.MasterKeyAllowedCIDRs)
	}
	if c.MasterKeyStatusURL != "http://fenix-agent:3000/internal/drive/users" || c.MasterKeyStatusTTL.Seconds() != 30 {
		t.Fatalf("status config: %q %v", c.MasterKeyStatusURL, c.MasterKeyStatusTTL)
	}
	if len(c.MasterKeyScopes) != 2 || c.MasterKeyScopes[1] != "drive.feedback" {
		t.Fatalf("scopes: %+v", c.MasterKeyScopes)
	}
}

func TestMasterKeyDefaults(t *testing.T) {
	baseConfigEnv(t)
	t.Setenv("DRIVE_MASTER_KEYS", `{"k1":"secret1"}`)
	c, e := Load()
	if e != nil {
		t.Fatal(e)
	}
	if c.MasterKeyStatusTTL.Seconds() != 60 || len(c.MasterKeyScopes) != 7 || c.MasterKeyScopes[0] != "drive.read" || c.MasterKeyScopes[6] != "drive.feedback" {
		t.Fatalf("master key defaults: %+v", c)
	}
	if len(c.MasterKeyAllowedCIDRs) != 0 {
		t.Fatalf("empty CIDR allowlist must deny every master key request: %+v", c.MasterKeyAllowedCIDRs)
	}
	if c.JobCredentialMode != "auto" || c.OCISServiceAudience != "ocis" || len(c.OCISServiceScopes) != 1 || c.OCISServiceScopes[0] != "drive.read" || c.JobCredentialTTL.Seconds() != 300 {
		t.Fatalf("job credential defaults: %+v", c)
	}
	if c.ExternalWriteEnabled || len(c.ExternalWriteIssuers) != 1 || c.ExternalWriteIssuers[0] != "fenix" || c.OCISWriteServiceTokenURL != "" || c.OCISWriteServiceAudience != "ocis" || len(c.OCISWriteServiceScopes) != 2 || c.OCISWriteServiceScopes[0] != "drive.read" || c.OCISWriteServiceScopes[1] != "drive.write" || c.OCISWriteServiceTTL.Seconds() != 300 || c.OCISWriteServiceVerified {
		t.Fatalf("external write defaults: %+v", c)
	}
	if len(c.ExternalSpaceIssuers) != 1 || c.ExternalSpaceIssuers[0] != "fenix" || c.SpaceProvisionerEnabled || c.SpaceProvisionerTokenURL != "" || c.SpaceProvisionerAudience != "ocis" || len(c.SpaceProvisionerScopes) != 1 || c.SpaceProvisionerScopes[0] != "openid" || c.SpaceProvisionerVerified || c.SpaceProvisionerNativeInstance != "ocis" {
		t.Fatalf("space provisioner defaults: %+v", c)
	}
}

func TestExternalWriteConfig(t *testing.T) {
	baseConfigEnv(t)
	t.Setenv("DRIVE_EXTERNAL_WRITE_ENABLED", "true")
	t.Setenv("DRIVE_EXTERNAL_WRITE_ISSUERS", "fenix,partner")
	t.Setenv("DRIVE_OCIS_WRITE_SERVICE_TOKEN_URL", "https://ocis.test/token")
	t.Setenv("DRIVE_OCIS_WRITE_SERVICE_CLIENT_ID", "write-client")
	t.Setenv("DRIVE_OCIS_WRITE_SERVICE_CLIENT_SECRET", "write-secret")
	t.Setenv("DRIVE_OCIS_WRITE_SERVICE_SCOPES", "drive.read drive.write")
	t.Setenv("DRIVE_OCIS_WRITE_SERVICE_TTL_SECONDS", "120")
	t.Setenv("DRIVE_OCIS_WRITE_SERVICE_VERIFIED", "true")
	c, e := Load()
	if e != nil {
		t.Fatal(e)
	}
	if !c.ExternalWriteEnabled || len(c.ExternalWriteIssuers) != 2 || c.ExternalWriteIssuers[1] != "partner" {
		t.Fatalf("external write enablement: %+v", c)
	}
	if len(c.OCISWriteServiceScopes) != 2 || c.OCISWriteServiceScopes[0] != "drive.read" || c.OCISWriteServiceScopes[1] != "drive.write" || c.OCISWriteServiceTTL.Seconds() != 120 || !c.OCISWriteServiceVerified {
		t.Fatalf("write service config: %+v", c)
	}
}

func TestExternalWriteConfigRejectsUnsafeValues(t *testing.T) {
	full := map[string]string{
		"DRIVE_EXTERNAL_WRITE_ENABLED":           "true",
		"DRIVE_OCIS_WRITE_SERVICE_TOKEN_URL":     "https://ocis.test/token",
		"DRIVE_OCIS_WRITE_SERVICE_CLIENT_ID":     "write-client",
		"DRIVE_OCIS_WRITE_SERVICE_CLIENT_SECRET": "write-secret",
		"DRIVE_OCIS_WRITE_SERVICE_VERIFIED":      "true",
	}
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"enabled without service", map[string]string{"DRIVE_EXTERNAL_WRITE_ENABLED": "true"}},
		{"enabled without verification", map[string]string{"DRIVE_EXTERNAL_WRITE_ENABLED": "true", "DRIVE_OCIS_WRITE_SERVICE_TOKEN_URL": "https://ocis.test/token", "DRIVE_OCIS_WRITE_SERVICE_CLIENT_ID": "write-client", "DRIVE_OCIS_WRITE_SERVICE_CLIENT_SECRET": "write-secret"}},
		{"enabled without issuers", map[string]string{"DRIVE_EXTERNAL_WRITE_ENABLED": "true", "DRIVE_EXTERNAL_WRITE_ISSUERS": " ", "DRIVE_OCIS_WRITE_SERVICE_TOKEN_URL": "https://ocis.test/token", "DRIVE_OCIS_WRITE_SERVICE_CLIENT_ID": "write-client", "DRIVE_OCIS_WRITE_SERVICE_CLIENT_SECRET": "write-secret", "DRIVE_OCIS_WRITE_SERVICE_VERIFIED": "true"}},
		{"partial service", map[string]string{"DRIVE_OCIS_WRITE_SERVICE_TOKEN_URL": "https://ocis.test/token"}},
		{"missing write scope", func() map[string]string {
			m := cloneEnv(full)
			m["DRIVE_OCIS_WRITE_SERVICE_SCOPES"] = "drive.read"
			return m
		}()},
		{"unknown write scope", func() map[string]string {
			m := cloneEnv(full)
			m["DRIVE_OCIS_WRITE_SERVICE_SCOPES"] = "drive.read,drive.write,drive.delete"
			return m
		}()},
		{"invalid token url", func() map[string]string {
			m := cloneEnv(full)
			m["DRIVE_OCIS_WRITE_SERVICE_TOKEN_URL"] = "file:///etc/passwd"
			return m
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseConfigEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, e := Load(); e == nil {
				t.Fatal("unsafe external write configuration accepted")
			}
		})
	}
}

func TestSpaceProvisionerConfig(t *testing.T) {
	baseConfigEnv(t)
	t.Setenv("DRIVE_SPACE_PROVISIONER_ENABLED", "true")
	t.Setenv("DRIVE_EXTERNAL_SPACE_ISSUERS", "fenix,partner")
	t.Setenv("DRIVE_SPACE_PROVISIONER_TOKEN_URL", "https://identity.test/token")
	t.Setenv("DRIVE_SPACE_PROVISIONER_CLIENT_ID", "provisioner-client")
	t.Setenv("DRIVE_SPACE_PROVISIONER_CLIENT_SECRET", "provisioner-secret")
	t.Setenv("DRIVE_SPACE_PROVISIONER_SCOPES", "openid")
	t.Setenv("DRIVE_SPACE_PROVISIONER_VERIFIED", "true")
	t.Setenv("DRIVE_SPACE_PROVISIONER_NATIVE_INSTANCE", "ocis")
	c, e := Load()
	if e != nil {
		t.Fatal(e)
	}
	if !c.SpaceProvisionerEnabled || !c.SpaceProvisionerVerified || c.SpaceProvisionerNativeInstance != "ocis" {
		t.Fatalf("space provisioner enablement: %+v", c)
	}
	if len(c.ExternalSpaceIssuers) != 2 || c.ExternalSpaceIssuers[1] != "partner" || len(c.SpaceProvisionerScopes) != 1 || c.SpaceProvisionerScopes[0] != "openid" {
		t.Fatalf("space provisioner lists: %+v", c)
	}
}

func TestSpaceProvisionerConfigRejectsUnsafeValues(t *testing.T) {
	full := map[string]string{
		"DRIVE_SPACE_PROVISIONER_ENABLED":       "true",
		"DRIVE_SPACE_PROVISIONER_TOKEN_URL":     "https://identity.test/token",
		"DRIVE_SPACE_PROVISIONER_CLIENT_ID":     "provisioner-client",
		"DRIVE_SPACE_PROVISIONER_CLIENT_SECRET": "provisioner-secret",
		"DRIVE_SPACE_PROVISIONER_VERIFIED":      "true",
	}
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"enabled without client", map[string]string{"DRIVE_SPACE_PROVISIONER_ENABLED": "true"}},
		{"enabled without verification", map[string]string{"DRIVE_SPACE_PROVISIONER_ENABLED": "true", "DRIVE_SPACE_PROVISIONER_TOKEN_URL": "https://identity.test/token", "DRIVE_SPACE_PROVISIONER_CLIENT_ID": "provisioner-client", "DRIVE_SPACE_PROVISIONER_CLIENT_SECRET": "provisioner-secret"}},
		{"enabled without issuers", map[string]string{"DRIVE_SPACE_PROVISIONER_ENABLED": "true", "DRIVE_EXTERNAL_SPACE_ISSUERS": " ", "DRIVE_SPACE_PROVISIONER_TOKEN_URL": "https://identity.test/token", "DRIVE_SPACE_PROVISIONER_CLIENT_ID": "provisioner-client", "DRIVE_SPACE_PROVISIONER_CLIENT_SECRET": "provisioner-secret", "DRIVE_SPACE_PROVISIONER_VERIFIED": "true"}},
		{"partial client", map[string]string{"DRIVE_SPACE_PROVISIONER_TOKEN_URL": "https://identity.test/token"}},
		{"invalid token url", func() map[string]string {
			m := cloneEnv(full)
			m["DRIVE_SPACE_PROVISIONER_TOKEN_URL"] = "file:///etc/passwd"
			return m
		}()},
		{"non-openid scope", func() map[string]string {
			m := cloneEnv(full)
			m["DRIVE_SPACE_PROVISIONER_SCOPES"] = "openid,drive.read"
			return m
		}()},
		{"empty scopes", func() map[string]string {
			m := cloneEnv(full)
			m["DRIVE_SPACE_PROVISIONER_SCOPES"] = " "
			return m
		}()},
		{"blank instance", func() map[string]string {
			m := cloneEnv(full)
			m["DRIVE_SPACE_PROVISIONER_NATIVE_INSTANCE"] = " "
			return m
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseConfigEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, e := Load(); e == nil {
				t.Fatal("unsafe space provisioner configuration accepted")
			}
		})
	}
}

func cloneEnv(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func TestMasterKeysFileWins(t *testing.T) {
	baseConfigEnv(t)
	p := filepath.Join(t.TempDir(), "master-keys")
	if e := os.WriteFile(p, []byte(`{"file-key":"file-secret"}`), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("DRIVE_MASTER_KEYS", `{"env-key":"env-secret"}`)
	t.Setenv("DRIVE_MASTER_KEYS_FILE", p)
	c, e := Load()
	if e != nil {
		t.Fatal(e)
	}
	if len(c.MasterKeys) != 1 || c.MasterKeys["file-key"] != "file-secret" {
		t.Fatalf("file must take precedence: %+v", c.MasterKeys)
	}
}

func TestMasterKeyConfigRejectsUnsafeValues(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"invalid json", map[string]string{"DRIVE_MASTER_KEYS": `{"k1":`}},
		{"empty secret", map[string]string{"DRIVE_MASTER_KEYS": `{"k1":""}`}},
		{"unsafe key id", map[string]string{"DRIVE_MASTER_KEYS": `{"k1\ninjected":"s"}`}},
		{"invalid cidr", map[string]string{"DRIVE_MASTER_KEYS": `{"k1":"s"}`, "DRIVE_MASTER_KEY_ALLOWED_CIDRS": "not-a-cidr"}},
		{"unknown default", map[string]string{"DRIVE_MASTER_KEYS": `{"k1":"s"}`, "DRIVE_MASTER_KEY_DEFAULT_ID": "k2"}},
		{"invalid status url", map[string]string{"DRIVE_MASTER_KEYS": `{"k1":"s"}`, "DRIVE_MASTER_KEY_STATUS_URL": "file:///etc/passwd"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseConfigEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, e := Load(); e == nil {
				t.Fatal("unsafe master key configuration accepted")
			}
		})
	}
}

func TestOCISServiceScopesAllowOpenIDForUserInfo(t *testing.T) {
	baseConfigEnv(t)
	t.Setenv("DRIVE_OCIS_SERVICE_TOKEN_URL", "http://ocis.test/token")
	t.Setenv("DRIVE_OCIS_SERVICE_CLIENT_ID", "id")
	t.Setenv("DRIVE_OCIS_SERVICE_CLIENT_SECRET", "secret")
	t.Setenv("DRIVE_OCIS_SERVICE_SCOPES", "openid,drive.read")
	c, e := Load()
	if e != nil {
		t.Fatal(e)
	}
	if len(c.OCISServiceScopes) != 2 || c.OCISServiceScopes[0] != "openid" || c.OCISServiceScopes[1] != "drive.read" {
		t.Fatalf("read service scopes wrong: %v", c.OCISServiceScopes)
	}
}

func TestJobCredentialConfigRejectsUnsafeValues(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"unknown mode", map[string]string{"DRIVE_JOB_CREDENTIAL_MODE": "magic"}},
		{"partial service", map[string]string{"DRIVE_OCIS_SERVICE_TOKEN_URL": "http://ocis.test/token"}},
		{"write scope", map[string]string{"DRIVE_OCIS_SERVICE_TOKEN_URL": "http://ocis.test/token", "DRIVE_OCIS_SERVICE_CLIENT_ID": "id", "DRIVE_OCIS_SERVICE_CLIENT_SECRET": "secret", "DRIVE_OCIS_SERVICE_SCOPES": "drive.read,drive.write"}},
		{"openid without read", map[string]string{"DRIVE_OCIS_SERVICE_TOKEN_URL": "http://ocis.test/token", "DRIVE_OCIS_SERVICE_CLIENT_ID": "id", "DRIVE_OCIS_SERVICE_CLIENT_SECRET": "secret", "DRIVE_OCIS_SERVICE_SCOPES": "openid"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseConfigEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, e := Load(); e == nil {
				t.Fatal("unsafe job credential configuration accepted")
			}
		})
	}
}
