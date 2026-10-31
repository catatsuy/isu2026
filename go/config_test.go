package isureal

import "testing"

func TestDBMaxOpenConnsFromEnv(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  int
	}{
		{"", 25},
		{"1", 1},
		{"50", 50},
	} {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("DB_MAXOPENCONNS", tt.value)
			if got := ConfigFromEnv().DBMaxOpenConns; got != tt.want {
				t.Fatalf("DBMaxOpenConns = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDBMaxOpenConnsRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"0", "-1", "invalid", "999999999999999999999999"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("DB_MAXOPENCONNS", value)
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic for invalid DB_MAXOPENCONNS")
				}
			}()
			ConfigFromEnv()
		})
	}
}
