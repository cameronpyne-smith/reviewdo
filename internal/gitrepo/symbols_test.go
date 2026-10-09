package gitrepo

import (
	"regexp"
	"strings"
	"testing"
)

func TestDeclarationPatterns(t *testing.T) {
	yes := []string{
		"public class MoveAsync",
		"    public async Task<int> MoveAsync(string serviceBusQueue, string rabbitMqQueue, CancellationToken cancellationToken = default)",
		"    public static BasicProperties MoveAsync(ServiceBusReceivedMessage message, string serviceBusQueue)",
		"func (p *Poller) MoveAsync(ctx context.Context) error {",
		"export function MoveAsync(a: string): void {",
		"const MoveAsync = async (a) => {",
		"pub fn MoveAsync(&self) -> Result<()> {",
		"def MoveAsync(self):",
		`resource "azurerm_resource_group" "MoveAsync" {`,
		"type MoveAsync struct {",
		"  MoveAsync:",
	}
	no := []string{
		"    await migration.MoveAsync(queue, rabbit, token);",
		"    var moved = MoveAsync(queue);",
		"// MoveAsync is called twice",
	}
	var res []*regexp.Regexp
	for _, p := range declarationPatterns("MoveAsync") {
		res = append(res, regexp.MustCompile(p))
	}
	match := func(s string) bool {
		for _, re := range res {
			if re.MatchString(s) {
				return true
			}
		}
		return false
	}
	for _, s := range yes {
		if !match(s) {
			t.Errorf("expected declaration: %q", s)
		}
	}
	for _, s := range no {
		if match(s) {
			t.Errorf("unexpected declaration: %q", s)
		}
	}
}

func TestOutline(t *testing.T) {
	src := strings.Join([]string{
		"using System;",
		"",
		"namespace Remundo.Reprocessor.RabbitMq;",
		"",
		"public class ServiceBusDeadLetterMigration",
		"{",
		"    private readonly AppSettings _appSettings;",
		"    public bool Applies(string a, string b) =>",
		"        !string.IsNullOrWhiteSpace(a);",
		"    public async Task<int> MoveAsync(string q, CancellationToken ct = default)",
		"    {",
		"        var waiting = await DeadLetterCountAsync(q, ct);",
		"        return 0;",
		"    }",
		"    public string Name { get; set; }",
		"}",
	}, "\n")
	out := Outline(src)
	for _, want := range []string{"3: namespace", "5: public class", "8:     public bool Applies", "10:     public async Task<int> MoveAsync", "15:     public string Name"} {
		if !strings.Contains(out, want) {
			t.Errorf("outline missing %q in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"var waiting", "return 0", "_appSettings"} {
		if strings.Contains(out, bad) {
			t.Errorf("outline should not contain %q in:\n%s", bad, out)
		}
	}
}
