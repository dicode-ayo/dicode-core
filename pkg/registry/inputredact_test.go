package registry

import "testing"

func TestShouldRedactName_ExactMatches(t *testing.T) {
	cases := []string{
		"Authorization",
		"authorization",
		"Cookie",
		"X-Hub-Signature-256",
		"X-Slack-Signature",
		"password",
		"PASSWORD",
		"api_key",
		"APIKEY",
		"bearer",
	}
	for _, name := range cases {
		if !shouldRedactName(name) {
			t.Errorf("shouldRedactName(%q) = false, want true", name)
		}
	}
}

func TestShouldRedactName_SubstringMatches(t *testing.T) {
	cases := []string{
		"x-custom-token",
		"MY_SLACK_TOKEN",
		"gh-secret-foo",
		"my_password_field",
		"public_key",
		"x-stripe-signature-v2",
	}
	for _, name := range cases {
		if !shouldRedactName(name) {
			t.Errorf("shouldRedactName(%q) = false, want true (substring match)", name)
		}
	}
}

func TestShouldRedactName_OverRedaction(t *testing.T) {
	// "tokens_per_minute" matches "token" substring — documented safe failure.
	if !shouldRedactName("tokens_per_minute") {
		t.Errorf("shouldRedactName(tokens_per_minute) = false; expected true (over-redaction is safe)")
	}
}

func TestShouldRedactName_NonMatches(t *testing.T) {
	cases := []string{
		"User-Agent",
		"Content-Type",
		"X-Request-ID",
		"name",
		"username",
		"email",
		"created_at",
	}
	for _, name := range cases {
		if shouldRedactName(name) {
			t.Errorf("shouldRedactName(%q) = true, want false", name)
		}
	}
}
