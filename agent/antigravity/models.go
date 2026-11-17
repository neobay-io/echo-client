package antigravity

import (
	"bufio"
	"bytes"
	"context"
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
	modelFetchTimeout   = 30 * time.Second
	modelRetryAfterFail = time.Minute
)

// agyModel is a base model with the effort variants `agy models` lists for it
// (gemini-3.8-flash-low, -medium, -high → gemini-3.8-flash + low/medium/high).
// agy accepts `--model <base> --effort <level>` but rejects a suffixed id
// combined with --effort.
type agyModel struct {
	ID      string
	Label   string
	Efforts []string
}

var effortOrder = map[string]int{"minimal": 0, "low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5}

// splitEffortSuffix splits "gemini-3.8-flash-low" into ("gemini-3.8-flash",
// "low"). Ids without a known effort suffix are returned unchanged.
func splitEffortSuffix(id string) (base, effort string) {
	idx := strings.LastIndex(id, "-")
	if idx <= 0 {
		return id, ""
	}
	if _, ok := effortOrder[strings.ToLower(id[idx+1:])]; ok {
		return id[:idx], strings.ToLower(id[idx+1:])
	}
	return id, ""
}

// parseAgyModels parses `agy models` output: one "id<TAB>label" per line,
// e.g. "gemini-3.8-flash-low	Gemini 3.8 Flash (Low)". Other lines (progress
// messages) are ignored. Order of first appearance is kept.
func parseAgyModels(out []byte) []agyModel {
	var models []agyModel
	index := map[string]int{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		id, label, ok := strings.Cut(scanner.Text(), "\t")
		id, label = strings.TrimSpace(id), strings.TrimSpace(label)
		if !ok || id == "" || strings.ContainsAny(id, " ") {
			continue
		}
		base, effort := splitEffortSuffix(id)
		if effort != "" {
			if open := strings.LastIndex(label, " ("); open > 0 && strings.HasSuffix(label, ")") &&
				strings.EqualFold(label[open+2:len(label)-1], effort) {
				label = label[:open]
			} else {
				// The label does not name the suffix, so it is part of the name.
				base, effort = id, ""
			}
		}
		i, seen := index[base]
		if !seen {
			index[base] = len(models)
			models = append(models, agyModel{ID: base, Label: label})
			i = len(models) - 1
		}
		if effort != "" {
			models[i].Efforts = append(models[i].Efforts, effort)
		}
	}
	for i := range models {
		sortEfforts(models[i].Efforts)
	}
	return models
}

func sortEfforts(efforts []string) {
	sort.SliceStable(efforts, func(i, j int) bool { return effortOrder[efforts[i]] < effortOrder[efforts[j]] })
}

// effortUnion returns every effort any model offers, lowest first.
func effortUnion(models []agyModel) []string {
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
	sortEfforts(out)
	return out
}

// modelArgs returns the model flags for a turn. With an effort, a suffixed id
// is reduced to its base because agy rejects "--model x-low --effort high".
func modelArgs(model, effort string) []string {
	var args []string
	if model != "" {
		if effort != "" {
			model, _ = splitEffortSuffix(model)
		}
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	return args
}

type modelFetcher func(ctx context.Context) ([]agyModel, error)

// modelCatalog caches `agy models` (it contacts the backend and takes a few
// seconds). get blocks on a refresh; cached never does.
type modelCatalog struct {
	fetch      modelFetcher
	mu         sync.Mutex
	models     []agyModel
	fetchedAt  time.Time
	failedAt   time.Time
	refreshing bool
}

func newModelCatalog(fetch modelFetcher) *modelCatalog {
	return &modelCatalog{fetch: fetch}
}

func (c *modelCatalog) get(ctx context.Context) []agyModel {
	c.mu.Lock()
	if c.freshLocked() {
		models := c.models
		c.mu.Unlock()
		return models
	}
	c.mu.Unlock()
	return c.refresh(ctx)
}

func (c *modelCatalog) cached() []agyModel {
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

func (c *modelCatalog) refresh(ctx context.Context) []agyModel {
	models, err := c.fetch(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshing = false
	if err != nil || len(models) == 0 {
		c.failedAt = time.Now()
		slog.Debug("antigravity: model discovery failed", "error", err, "models", len(models))
		return c.models
	}
	c.models, c.fetchedAt = models, time.Now()
	return models
}

func (a *Agent) fetchModels(ctx context.Context) ([]agyModel, error) {
	a.mu.Lock()
	cmdPath := a.cmd
	args := append(append([]string(nil), a.cliExtraArgs...), "models")
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
		return nil, fmt.Errorf("agy models: %w", err)
	}
	return parseAgyModels(out), nil
}
