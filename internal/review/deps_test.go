package review

import (
	"context"
	"strings"
	"testing"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
)

func TestParseManifest(t *testing.T) {
	cases := []struct {
		name, text string
		want       []string
	}{
		{"X.csproj", `<Project><ItemGroup><PackageReference Include="RabbitMQ.Client" Version="7.1.2" /><PackageReference Include="NLog" /></ItemGroup></Project>`, []string{"RabbitMQ.Client 7.1.2", "NLog"}},
		{"package.json", `{"name":"x","dependencies":{"svelte":"^5.0.0"},"devDependencies":{"vitest":"2.1.0"}}`, []string{"svelte ^5.0.0", "vitest 2.1.0"}},
		{"go.mod", "module x\n\ngo 1.22\n\nrequire (\n\tgithub.com/a/b v1.2.3\n\tgolang.org/x/net v0.1.0 // indirect\n)\n", []string{"github.com/a/b v1.2.3", "golang.org/x/net v0.1.0"}},
		{"Cargo.toml", "[package]\nname = \"x\"\n\n[dependencies]\ntokio = { version = \"1.40\", features = [\"full\"] }\nserde = \"1.0\"\n\n[dev-dependencies]\nrstest = \"0.22\"\n", []string{"tokio 1.40", "serde 1.0", "rstest 0.22"}},
		{"requirements.txt", "# deps\nrequests==2.32.0\nnumpy>=1.26\n-r other.txt\n", []string{"requests ==2.32.0", "numpy >=1.26"}},
	}
	for _, c := range cases {
		got := parseManifest(c.name, c.text)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestDependenciesWalksUpFromChangedFiles(t *testing.T) {
	repo := treeRepo{
		"src/App/App.csproj":       `<Project><ItemGroup><PackageReference Include="RabbitMQ.Client" Version="7.1.2" /></ItemGroup></Project>`,
		"src/App/Service.cs":       "class Service {}",
		"src/Other/Other.csproj":   `<Project><ItemGroup><PackageReference Include="Serilog" Version="4.0.0" /></ItemGroup></Project>`,
		"Directory.Packages.props": `<Project><ItemGroup><PackageVersion Include="xunit" Version="2.9.0" /></ItemGroup></Project>`,
	}
	out := Dependencies(context.Background(), repo, []*diff.File{{Path: "src/App/Service.cs"}})
	for _, want := range []string{"src/App/App.csproj: RabbitMQ.Client 7.1.2", "Directory.Packages.props: xunit 2.9.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Serilog") {
		t.Errorf("manifest outside the changed path listed:\n%s", out)
	}
}
