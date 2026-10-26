package config
import("testing";"os";"path/filepath")
func TestDefaultsAndStorage(t *testing.T){t.Setenv("DRIVE_DATABASE_URL","postgres://example");t.Setenv("DRIVE_STORAGE_MODE","local");c,e:=Load();if e!=nil||c.UploadMax!=20<<30||c.ConvertMax!=100<<20||c.AllowUnverifiedOCIS{t.Fatalf("unsafe config: %+v %v",c,e)};t.Setenv("DRIVE_STORAGE_MODE","magic");if _,e=Load();e==nil{t.Fatal("invalid storage allowed")}}
func TestSecretFile(t *testing.T){p:=filepath.Join(t.TempDir(),"secret");os.WriteFile(p,[]byte(" secret\n"),0600);t.Setenv("TEST_SECRET_FILE",p);v,e:=Secret("TEST_SECRET");if e!=nil||v!="secret"{t.Fatal(v,e)}}
