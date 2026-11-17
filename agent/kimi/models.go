package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const (
	modelCatalogTTL     = 10 * time.Minute
	modelFetchTimeout   = 20 * time.Second
	modelRetryAfterFail = time.Minute
)

// kimiModel is one model alias from `kimi provider list --json`.
type kimiModel struct {
	Alias         string
	DisplayName   string
	Efforts       []string
	DefaultEffort string
}

type modelFetcher func(ctx context.Context) ([]kimiModel, error)

// modelCatalog caches the account's model aliases. get blocks on a refresh;
// cached never does (it starts one in the background instead) so cheap paths
// such as capability reporting do not spawn the CLI synchronously.
type modelCatalog struct {
	fetch      modelFetcher
	mu         sync.Mutex
	models     []kimiModel
	fetchedAt  time.Time
	failedAt   time.Time
	refreshing bool
}

func newModelCatalog(fetch modelFetcher) *modelCatalog {
	return &modelCatalog{fetch: fetch}
}

func (c *modelCatalog) get(ctx context.Context) []kimiModel {
	c.mu.Lock()
	if c.freshLocked() {
		models := c.models
		c.mu.Unlock()
		return models
	}
	c.mu.Unlock()
	return c.refresh(ctx)
}

func (c *modelCatalog) cached() []kimiModel {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.freshLocked() && !c.refreshing && time.Since(c.failedAt) > modelRetryAfterFail {
		c.refreshing = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), modelFetchTimeout)
			defer cancel()
			c.refresh(ctx)
		}()
	}
	return c.models
}

func (c *modelCatalog) freshLocked() bool {
	return !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < modelCatalogTTL
}

func (c *modelCatalog) refresh(ctx context.Context) []kimiModel {
	models, err := c.fetch(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshing = false
	if err != nil {
		c.failedAt = time.Now()
		slog.Debug("kimi: model discovery failed", "error", err)
		return c.models // keep the last good list
	}
	c.models, c.fetchedAt = models, time.Now()
	return models
}

// fetchModels runs `kimi provider list --json`. Its output also contains
// provider credentials, so it is decoded into a narrow struct and never
// logged, and exec errors are reported without stderr.
func (a *Agent) fetchModels(ctx context.Context) ([]kimiModel, error) {
	a.mu.Lock()
	cmdPath := a.cmd
	args := append(append([]string(nil), a.cliExtraArgs...), "provider", "list", "--json")
	workDir := a.workDir
	env := core.MergeEnv(os.Environ(), a.configEnv)
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, modelFetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cmdPath, args...)
	cmd.Dir = workDir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("provider list exited with %d", exitErr.ExitCode())
		}
		return nil, fmt.Errorf("provider list: %w", err)
	}
	return parseKimiModels(out)
}

func parseKimiModels(data []byte) ([]kimiModel, error) {
	var catalog struct {
		Models map[string]struct {
			DisplayName   string   `json:"displayName"`
			SupportEffort []string `json:"supportEfforts"`
			DefaultEffort string   `json:"defaultEffort"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("decode provider list: %w", err)
	}
	models := make([]kimiModel, 0, len(catalog.Models))
	for alias, m := range catalog.Models {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		display := strings.TrimSpace(m.DisplayName)
		if display == alias {
			display = ""
		}
		var efforts []string
		for _, e := range m.SupportEffort {
			if e = normalizeEffort(e); e != "" {
				efforts = append(efforts, e)
			}
		}
		models = append(models, kimiModel{
			Alias:         alias,
			DisplayName:   display,
			Efforts:       efforts,
			DefaultEffort: normalizeEffort(m.DefaultEffort),
		})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Alias < models[j].Alias })
	return models, nil
}

var effortOrder = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// effortUnion returns every effort any model supports, lowest first.
func effortUnion(models []kimiModel) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range models {
		for _, e := range m.Efforts {
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		oi, iok := effortOrder[out[i]]
		oj, jok := effortOrder[out[j]]
		if iok != jok {
			return iok
		}
		return oi < oj
	})
	return out
}
