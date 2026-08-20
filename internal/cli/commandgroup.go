package cli

import "strings"

// subcommandDrivers are commands whose second word carries the real meaning
// ("go test" vs "go build"). Add new entries here.
var subcommandDrivers = map[string]bool{
	"go":        true,
	"npm":       true,
	"yarn":      true,
	"pnpm":      true,
	"npx":       true,
	"docker":    true,
	"git":       true,
	"cargo":     true,
	"make":      true,
	"kubectl":   true,
	"terraform": true,
	"bundle":    true,
	"uv":        true,
	"gh":        true,
	"brew":      true,
	"poetry":    true,
	"rake":      true,
	"helm":      true,
	"pip":       true,
}

// commandGroup buckets a span's argv for faceting and reverse links. Known
// subcommand drivers keep two words; everything else collapses to argv[0], so
// "pytest -q tests/" and "pytest tests/x -x" share one group.
func commandGroup(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	base := commandBase(argv[0])
	if base == "" {
		return ""
	}
	if !subcommandDrivers[base] {
		return base
	}
	for _, a := range argv[1:] {
		if a == "" || strings.HasPrefix(a, "-") {
			continue
		}
		return base + " " + a
	}
	return base
}

// commandBase strips any directory prefix so "./scripts/deploy.sh" and
// "/usr/bin/go" group by their leaf name.
func commandBase(arg0 string) string {
	if i := strings.LastIndexAny(arg0, `/\`); i >= 0 {
		return arg0[i+1:]
	}
	return arg0
}
