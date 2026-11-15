package load

import "testing"

// TestCheckWorkspaceName checks the rules of a workspace's name, and that a name breaking one names the rule.
func TestCheckWorkspaceName(t *testing.T) {
	for _, valid := range []string{"storefront", "com.acme.storefront", "my-app_2", "a", "a1", "x.y-z_0"} {
		if err := checkWorkspaceName(valid); err != nil {
			t.Errorf("checkWorkspaceName(%q) = %v, want nil", valid, err)
		}
	}
	for _, example := range []struct {
		name  string
		error string
	}{
		{
			name:  "",
			error: "name must not be empty",
		},
		{
			name:  "Storefront",
			error: `name "Storefront" must start with a lowercase letter`,
		},
		{
			name:  "2app",
			error: `name "2app" must start with a lowercase letter`,
		},
		{
			name:  "_app",
			error: `name "_app" must start with a lowercase letter`,
		},
		{
			name: "acme/app",
			error: `name "acme/app" must not contain "/"; a workspace name holds only lowercase letters, digits, ` +
				`".", "-" and "_"`,
		},
		{
			name: "acmeApp",
			error: `name "acmeApp" must not contain "A"; a workspace name holds only lowercase letters, digits, ` +
				`".", "-" and "_"`,
		},
		{
			name: "café",
			error: `name "café" must not contain "é"; a workspace name holds only lowercase letters, digits, ` +
				`".", "-" and "_"`,
		},
		{
			name: "caf\xe9",
			error: `name "caf\xe9" must not contain "\xe9"; a workspace name holds only lowercase letters, digits, ` +
				`".", "-" and "_"`,
		},
		{
			name: "app\ufffd",
			error: "name \"app\ufffd\" must not contain \"\ufffd\"; a workspace name holds only lowercase letters, " +
				`digits, ".", "-" and "_"`,
		},
		{
			name:  "app-",
			error: `name "app-" must end with a lowercase letter or digit`,
		},
		{
			name:  "app.",
			error: `name "app." must end with a lowercase letter or digit`,
		},
		{
			name:  "app_",
			error: `name "app_" must end with a lowercase letter or digit`,
		},
	} {
		err := checkWorkspaceName(example.name)
		if err == nil || err.Error() != example.error {
			t.Errorf("checkWorkspaceName(%q) = %v, want %s", example.name, err, example.error)
		}
	}
}
