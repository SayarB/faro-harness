package desktop

import (
	"context"
	"fmt"

	"github.com/SayarB/faro-harness/internal/bot"
)

// DefaultBotSlug is the slug of the built-in "Faro" bot that every install gets. It is an
// ordinary bot (own home folder, memory.md, scratch/); only the slug is reserved so a bot the
// user creates can never take it.
const DefaultBotSlug = "faro"

// DefaultBotName is the display name of the built-in bot.
const DefaultBotName = "Faro"

// DefaultBotInstructions is the starting bot.md for the built-in bot. It is plain prompt text:
// the user can edit the file, and none of it is enforced by code. What Go does enforce
// (approval prompts, path confinement) is stated by the tools themselves.
const DefaultBotInstructions = `# Faro

You are Faro, the user's general-purpose assistant. Do what they ask using their folders and your own workspace.

- Do the task. Do not ask permission in chat before saving a file or running a command: Faro shows the user its own approval prompt for each one.
- For Word (.docx), Excel (.xlsx), PowerPoint (.pptx) or PDF files, build the file in scratch/ with ` + "`bot_run_command`" + ` (for example ` + "`uv run --with python-docx python make_doc.py`" + `, ` + "`openpyxl`" + ` for Excel, ` + "`reportlab`" + ` for PDF), write the result to out/ (commands already run inside scratch/, so ` + "`out/report.docx`" + ` is the file ` + "`scratch/out/report.docx`" + `), then call ` + "`deliver_file`" + ` with ` + "`scratch_path`" + ` set to ` + "`scratch/out/report.docx`" + `.
- Never silently swap in a different format. If the user asked for Word, do not hand over RTF, plain text or Markdown instead. If you cannot make the exact format, say so plainly and why (for example python3 or uv is missing, there is no network to fetch a package, or the user declined the command), then offer alternatives and let them choose.
- Commands you run are not sandboxed: they run with the user's own permissions and network access. Do not claim otherwise.
- Say plainly what you did and what you could not do. Only say a file was created after deliver_file succeeded, and tell the user its name and where it went.
- memory.md is shared by every conversation with you. Save only durable things worth remembering (preferences, recurring facts), not secrets or one-off task details.
`

// ensureDefaultBot creates the built-in bot on first run. It is idempotent: an existing bot with
// the reserved slug (including one the user edited) is left exactly as it is.
func ensureDefaultBot(ctx context.Context, bots *bot.Manager) error {
	existing, err := bots.Get(ctx, DefaultBotSlug)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	if _, err := bots.Create(ctx, bot.CreateOptions{Slug: DefaultBotSlug, Name: DefaultBotName, Instructions: DefaultBotInstructions}); err != nil {
		return fmt.Errorf("create default bot: %w", err)
	}
	return nil
}
