package license

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/ext"
	"github.com/trebi-ai/trebi/internal/home"
)

type fakeVerifier struct {
	verify func(raw []byte, now time.Time) (ext.Entitlement, error)
}

func (f fakeVerifier) Verify(raw []byte, now time.Time) (ext.Entitlement, error) {
	return f.verify(raw, now)
}

func TestHasWithoutVerifier(t *testing.T) {
	s := newTestService(t, nil)
	if !s.Has(FeatureEmail) {
		t.Error("unenforced feature must be allowed without a verifier")
	}
	if FeatureEmail.Enforced() {
		t.Error("no feature is enforced today")
	}
}

func TestHasNilVerifierTruthTable(t *testing.T) {
	features := []Feature{FeatureEmail, FeatureRemoteAccess, FeatureHostedIngress, FeatureBackup}
	s := newTestService(t, nil)
	for _, f := range features {
		if f.Enforced() {
			t.Fatalf("%s is enforced in the release table; this test assumes all false", f)
		}
		if !s.Has(f) {
			t.Errorf("unenforced %s: Has=false", f)
		}
	}
	for _, f := range features {
		t.Run(string(f)+"/enforced", func(t *testing.T) {
			enforce(t, f, true)
			svc := newTestService(t, nil)
			if svc.Has(f) {
				t.Errorf("enforced %s with nil verifier: Has=true", f)
			}
		})
	}
}

func TestHasWithEntitlement(t *testing.T) {
	enforce(t, FeatureEmail, true)
	enforce(t, FeatureBackup, true)
	v := fakeVerifier{verify: func([]byte, time.Time) (ext.Entitlement, error) {
		return ext.Entitlement{Plan: "pro", Features: []string{string(FeatureEmail)}}, nil
	}}
	s := newTestService(t, v)
	if err := os.WriteFile(home.LicensePath(), []byte("signed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !s.Has(FeatureEmail) {
		t.Error("granted feature must be allowed")
	}
	if s.Has(FeatureBackup) {
		t.Error("enforced feature absent from the entitlement must be refused")
	}
}

func TestInvalidLicenseFile(t *testing.T) {
	enforce(t, FeatureEmail, true)
	v := fakeVerifier{verify: func([]byte, time.Time) (ext.Entitlement, error) {
		return ext.Entitlement{}, ext.ErrInvalid
	}}
	s := newTestService(t, v)
	if err := os.WriteFile(home.LicensePath(), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.Has(FeatureEmail) {
		t.Error("an invalid file must not grant an enforced feature")
	}
	st := s.State()
	if st.Source != "none" || st.Plan != "" {
		t.Errorf("state after invalid file: %+v", st)
	}
}

func TestStateAndExpiry(t *testing.T) {
	exp := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	v := fakeVerifier{verify: func([]byte, time.Time) (ext.Entitlement, error) {
		return ext.Entitlement{Plan: "pro", Features: []string{"email"}, Subject: "Fritz", ExpiresAt: exp}, nil
	}}
	s := newTestService(t, v)
	if err := os.WriteFile(home.LicensePath(), []byte("signed"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := s.State()
	if !st.Supported || st.Source != "file" || st.Plan != "pro" || st.Subject != "Fritz" {
		t.Errorf("state: %+v", st)
	}
	if st.ExpiresAt == nil || !st.ExpiresAt.Equal(exp) {
		t.Errorf("expiry: %+v", st.ExpiresAt)
	}
}

func TestInstall(t *testing.T) {
	t.Run("refuses without a verifier", func(t *testing.T) {
		s := newTestService(t, nil)
		if err := s.Install([]byte("x")); !errors.Is(err, ErrNoVerifier) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("rejects an invalid key and writes nothing", func(t *testing.T) {
		v := fakeVerifier{verify: func([]byte, time.Time) (ext.Entitlement, error) {
			return ext.Entitlement{}, ext.ErrInvalid
		}}
		s := newTestService(t, v)
		if err := s.Install([]byte("bad")); err == nil {
			t.Error("invalid key must be refused")
		}
		if _, err := os.Stat(home.LicensePath()); !os.IsNotExist(err) {
			t.Error("no file must be written for an invalid key")
		}
	})

	t.Run("writes, reloads, and grants", func(t *testing.T) {
		enforce(t, FeatureEmail, true)
		v := fakeVerifier{verify: func(raw []byte, _ time.Time) (ext.Entitlement, error) {
			if string(raw) == "good" {
				return ext.Entitlement{Plan: "pro", Features: []string{"email"}}, nil
			}
			return ext.Entitlement{}, ext.ErrInvalid
		}}
		s := newTestService(t, v)
		if s.Has(FeatureEmail) {
			t.Fatal("pre-install Has must refuse an enforced feature")
		}
		if err := s.Install([]byte("good")); err != nil {
			t.Fatal(err)
		}
		if !s.Has(FeatureEmail) {
			t.Error("installed license must grant email")
		}
		if !s.Has(FeatureBackup) {
			t.Error("unenforced backup stays allowed")
		}
		if got, err := os.ReadFile(home.LicensePath()); err != nil || string(got) != "good" {
			t.Errorf("file = %q, %v", got, err)
		}
	})
}

func TestReloadSeesFileChanges(t *testing.T) {
	enforce(t, FeatureEmail, true)
	reject := true
	v := fakeVerifier{verify: func([]byte, time.Time) (ext.Entitlement, error) {
		if reject {
			return ext.Entitlement{}, ext.ErrInvalid
		}
		return ext.Entitlement{Plan: "pro", Features: []string{"email"}}, nil
	}}
	s := newTestService(t, v)
	if err := os.WriteFile(home.LicensePath(), []byte("signed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.Has(FeatureEmail) {
		t.Fatal("a rejected license must not grant an enforced feature")
	}
	reject = false
	s.Reload()
	if !s.Has(FeatureEmail) {
		t.Error("reload must re-read the file")
	}
}

func newTestService(t *testing.T, v ext.LicenseVerifier) *Service {
	t.Helper()
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	return New(v)
}

func enforce(t *testing.T, f Feature, on bool) {
	t.Helper()
	enforced[f] = on
	t.Cleanup(func() { enforced[f] = !on })
}
