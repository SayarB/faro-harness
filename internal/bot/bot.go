// Package bot manages Faro's isolated per-bot folders (persona, memory, scratchpad).
package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/SayarB/faro-harness/internal/store"
)

const (
	BotFileName       = "bot.md"
	MemoryFileName    = "memory.md"
	ScratchDirName    = "scratch"
	ScratchOutDirName = "out"
	MaxBotMDBytes     = 8 * 1024
	MaxMemoryMDBytes  = 12 * 1024
	MaxScratchList    = 30
)

// CreateOptions configures a newly spun-up bot.
type CreateOptions struct {
	Slug         string
	Name         string
	Instructions string
	Model        string
}

// Profile is the point-in-time snapshot of a bot's own folder loaded into its context.
type Profile struct {
	Bot             store.Bot
	Instructions    string
	Memory          string
	MemoryTruncated bool
	ScratchFiles    []string
}

// Manager creates, loads, and lists isolated bots backed by SQLite + per-bot home folders.
type Manager struct {
	store   *store.Store
	botsDir string
}

// NewManager creates a bot Manager storing bot folders under botsDir.
// If botsDir is empty, defaults to store.DefaultBotsDir().
func NewManager(st *store.Store, botsDir string) (*Manager, error) {
	if st == nil {
		return nil, errors.New("store is required")
	}
	if strings.TrimSpace(botsDir) == "" {
		var err error
		botsDir, err = store.DefaultBotsDir()
		if err != nil {
			return nil, fmt.Errorf("resolve bots dir: %w", err)
		}
	}
	absDir, err := filepath.Abs(botsDir)
	if err != nil {
		return nil, fmt.Errorf("resolve bots dir %q: %w", botsDir, err)
	}
	return &Manager{
		store:   st,
		botsDir: absDir,
	}, nil
}

// BotsDir returns the root directory containing all bot home folders.
func (m *Manager) BotsDir() string {
	return m.botsDir
}

// Create provisions a new bot's isolated home folder (bot.md, memory.md, scratch/)
// and registers it in SQLite.
func (m *Manager) Create(ctx context.Context, opts CreateOptions) (store.Bot, error) {
	slug, err := NormalizeSlug(opts.Slug)
	if err != nil {
		return store.Bot{}, err
	}
	if existing, err := m.store.GetBotBySlug(ctx, slug); err != nil {
		return store.Bot{}, err
	} else if existing != nil {
		return store.Bot{}, fmt.Errorf("bot %q already exists (id=%s)", slug, existing.ID)
	}

	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = defaultDisplayName(slug)
	}

	homePath := filepath.Join(m.botsDir, slug)
	if _, err := os.Stat(homePath); err == nil {
		return store.Bot{}, fmt.Errorf("bot folder %q already exists on disk; remove or rename it before creating bot %q", homePath, slug)
	} else if !os.IsNotExist(err) {
		return store.Bot{}, fmt.Errorf("stat bot folder %q: %w", homePath, err)
	}
	scratchOutPath := filepath.Join(homePath, ScratchDirName, ScratchOutDirName)
	if err := os.MkdirAll(scratchOutPath, 0o755); err != nil {
		return store.Bot{}, fmt.Errorf("create bot folder %q: %w", homePath, err)
	}
	createdOK := false
	defer func() {
		if !createdOK {
			_ = os.RemoveAll(homePath)
		}
	}()

	botMD := strings.TrimSpace(opts.Instructions)
	if botMD == "" {
		botMD = fmt.Sprintf("# %s\n\nYou are %s, a dedicated local assistant helping the user with their files.\n", name, name)
	} else if !strings.HasSuffix(botMD, "\n") {
		botMD += "\n"
	}

	botMDPath := filepath.Join(homePath, BotFileName)
	if err := os.WriteFile(botMDPath, []byte(botMD), 0o644); err != nil {
		return store.Bot{}, fmt.Errorf("write %s: %w", BotFileName, err)
	}

	memPath := filepath.Join(homePath, MemoryFileName)
	starterMem := "# Memory\n\n(No saved notes yet.)\n"
	if err := os.WriteFile(memPath, []byte(starterMem), 0o644); err != nil {
		return store.Bot{}, fmt.Errorf("write %s: %w", MemoryFileName, err)
	}

	id, err := NewID("bot")
	if err != nil {
		return store.Bot{}, err
	}
	created, err := m.store.CreateBot(ctx, store.Bot{
		ID:       id,
		Slug:     slug,
		Name:     name,
		HomePath: homePath,
		Model:    strings.TrimSpace(opts.Model),
	})
	if err != nil {
		return store.Bot{}, err
	}
	createdOK = true
	return created, nil
}

// Get looks up a bot by slug or ID.
func (m *Manager) Get(ctx context.Context, slugOrID string) (*store.Bot, error) {
	key := strings.TrimSpace(slugOrID)
	if key == "" {
		return nil, errors.New("bot slug or id is required")
	}
	b, err := m.store.GetBotBySlug(ctx, strings.ToLower(key))
	if err != nil || b != nil {
		return b, err
	}
	return m.store.GetBotByID(ctx, key)
}

// List returns all registered bots.
func (m *Manager) List(ctx context.Context) ([]store.Bot, error) {
	return m.store.ListBots(ctx)
}

// LoadProfile reads only this bot's own bot.md, memory.md, and scratch/ filenames.
// As a deliberate self-healing step, it ensures scratch/out/ exists on disk.
func LoadProfile(b store.Bot) (Profile, error) {
	if strings.TrimSpace(b.HomePath) == "" {
		return Profile{}, errors.New("bot home path is empty")
	}
	// Ensure scratch/out/ exists even if the user manually cleaned it up.
	if err := os.MkdirAll(filepath.Join(b.HomePath, ScratchDirName, ScratchOutDirName), 0o755); err != nil {
		return Profile{}, fmt.Errorf("ensure scratch dir: %w", err)
	}

	instr, _, err := readBoundedFile(filepath.Join(b.HomePath, BotFileName), MaxBotMDBytes)
	if err != nil && !os.IsNotExist(err) {
		return Profile{}, fmt.Errorf("read %s: %w", BotFileName, err)
	}

	mem, memTrunc, err := readBoundedFile(filepath.Join(b.HomePath, MemoryFileName), MaxMemoryMDBytes)
	if err != nil && !os.IsNotExist(err) {
		return Profile{}, fmt.Errorf("read %s: %w", MemoryFileName, err)
	}

	scratchFiles := listScratchFiles(filepath.Join(b.HomePath, ScratchDirName), MaxScratchList)
	return Profile{
		Bot:             b,
		Instructions:    strings.TrimSpace(instr),
		Memory:          strings.TrimSpace(mem),
		MemoryTruncated: memTrunc,
		ScratchFiles:    scratchFiles,
	}, nil
}

// NormalizeSlug validates and normalizes a bot slug (lowercase letters, digits, '-' and '_').
func NormalizeSlug(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "", errors.New("bot slug is required")
	}
	if len(s) > 48 {
		return "", fmt.Errorf("bot slug %q is too long (max 48 chars)", s)
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (i > 0 && (r == '-' || r == '_'))
		if !ok {
			return "", fmt.Errorf("invalid bot slug %q: use lowercase letters, digits, '-' or '_'", raw)
		}
	}
	return s, nil
}

// NewID generates a random URL-safe prefixed identifier (e.g. "bot_a1b2c3...").
func NewID(prefix string) (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(raw[:]), nil
}

func defaultDisplayName(slug string) string {
	parts := strings.FieldsFunc(slug, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		runes := []rune(p)
		if len(runes) > 0 {
			runes[0] = unicode.ToUpper(runes[0])
			parts[i] = string(runes)
		}
	}
	return strings.Join(parts, " ")
}

func readBoundedFile(path string, maxBytes int) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()

	buf := make([]byte, maxBytes+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", false, err
	}
	truncated := n > maxBytes
	if truncated {
		n = maxBytes
		for n > 0 && (buf[n]&0xC0) == 0x80 {
			n--
		}
	}
	return string(buf[:n]), truncated, nil
}

func listScratchFiles(scratchDir string, maxEntries int) []string {
	type scratchItem struct {
		rel   string
		mtime int64
	}
	var items []scratchItem
	_ = filepath.WalkDir(scratchDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path == scratchDir {
				return nil
			}
			// Skip hidden/cache/venv/dependency directories so scratch listings stay focused on outputs.
			if strings.HasPrefix(name, ".") || name == "__pycache__" || name == "node_modules" || name == "venv" || name == "env" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		rel, err := filepath.Rel(scratchDir, path)
		if err != nil {
			return nil
		}
		var mtime int64
		if info, ierr := d.Info(); ierr == nil {
			mtime = info.ModTime().UnixNano()
		}
		items = append(items, scratchItem{
			rel:   filepath.ToSlash(rel),
			mtime: mtime,
		})
		return nil
	})
	sort.Slice(items, func(i, j int) bool {
		if items[i].mtime != items[j].mtime {
			return items[i].mtime > items[j].mtime
		}
		return items[i].rel < items[j].rel
	})
	if len(items) > maxEntries {
		items = items[:maxEntries]
	}
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.rel
	}
	return out
}
