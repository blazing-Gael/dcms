package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// newInitCmd scaffolds a ready-to-run project: a starter schema, a config with a
// dev-friendly CORS default, a .env template for the secret admin credentials,
// and a .gitignore. The goal is that a freshly-installed binary goes from `dcms
// init` to a working backend with `dcms dev` in two commands — no schema to write
// by hand first, and no cross-origin surprise when a separate frontend calls it.
func newInitCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Scaffold a new DCMS project (schema, config, .env) ready to run",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create project dir: %w", err)
			}

			files := []struct {
				name    string
				content string
			}{
				{"dcms.schema.yaml", starterSchema},
				{"dcms.config.yaml", starterConfig},
				{".env.example", starterEnvExample},
				{".gitignore", starterGitignore},
			}

			// Refuse to clobber an existing project unless --force, so re-running
			// init in a real project never silently overwrites work.
			if !force {
				for _, f := range files {
					if _, err := os.Stat(filepath.Join(dir, f.name)); err == nil {
						return fmt.Errorf("%s already exists (use --force to overwrite)", filepath.Join(dir, f.name))
					}
				}
			}
			for _, f := range files {
				if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.content), 0o644); err != nil {
					return fmt.Errorf("write %s: %w", f.name, err)
				}
			}

			printInitNextSteps(cmd.OutOrStdout(), dir)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing files")
	return cmd
}

// printInitNextSteps tells the user exactly what to type next. The admin
// credentials are secrets, so they come from the environment, never a file the
// scaffold writes — the .env.example shows the shape.
func printInitNextSteps(w interface{ Write([]byte) (int, error) }, dir string) {
	cd := ""
	if dir != "." {
		cd = fmt.Sprintf("  cd %s\n", dir)
	}
	fmt.Fprintf(w, `Scaffolded a DCMS project in %s

  dcms.schema.yaml   your content model (a starter "posts" collection)
  dcms.config.yaml   server settings; dev CORS is pre-set for localhost frontends
  .env.example       copy to .env and set the first admin's credentials
  .gitignore         ignores the database, media, and .env

Next:
%s  cp .env.example .env      # then edit .env: set DCMS_ADMIN_EMAIL and DCMS_ADMIN_PASSWORD
  dcms dev                  # migrates, seeds the admin, and serves the API

Your backend will be at http://localhost:8080
  REST API      http://localhost:8080/api/v1
  API docs      http://localhost:8080/__docs
  Log in        POST http://localhost:8080/auth/login

Point your frontend (http://localhost:3000 or :5173) at it — those origins are
already allowed in dcms.config.yaml. Edit the schema and restart to change your model.
`, dir, cd)
}

const starterSchema = `version: "1"

meta:
  name: myapp
  description: "A DCMS backend — edit this schema to model your content."
  base_url: /api/v1

# Roles your access: rules can reference. The first admin is seeded on first run
# from DCMS_ADMIN_EMAIL / DCMS_ADMIN_PASSWORD (see .env.example), or create one
# with:  dcms admin create --email you@example.com
auth:
  provider: local
  session:
    ttl: 168h            # 7 days
  roles:
    admin:
      label: "Administrator"

collections:
  # A simple blog post. Anyone may read; only admins may write. Delete what you
  # don't need and add your own collections — the REST API and the OpenAPI docs
  # regenerate automatically the next time you run 'dcms dev'.
  posts:
    fields:
      title:
        type: string
        required: true
        max: 200
      slug:
        type: string
        required: true
        unique: true
        pattern: "^[a-z0-9-]+$"
      body:
        type: richtext     # structured rich text (sensible default styles/marks)
      excerpt:
        type: text
      cover:
        type: file         # an uploaded image, stored in the media library
      status:
        type: enum
        values: [draft, published]
        default: draft
    timestamps: true
    indexes: [slug, status]
    access:
      read: public         # storefront/blog reads are anonymous
      create: [admin]
      update: [admin]
      delete: [admin]
`

const starterConfig = `# DCMS config. Every setting is optional; anything omitted falls back to a
# built-in default, and any value can be overridden by a DCMS_* env var or a
# command-line flag at launch. See examples/dcms.config.yaml in the repo for the
# fully-annotated version with every option.

schema: ./dcms.schema.yaml

database:
  driver: sqlite
  path: ./dcms.db          # created on first run; this file IS your data

server:
  # 8080 keeps the API clear of common frontend dev servers (3000, 5173).
  port: 8080

  # Cross-origin access for a separate frontend. A browser app served from
  # another origin (your dev server) is blocked by CORS unless its origin is
  # listed here — these are the usual local dev origins. Add your production
  # frontend origin (https://yoursite.com) before you deploy, and drop any you
  # don't use. allow_credentials lets cookie-based sessions work cross-origin;
  # with it on, origins must be explicit (a wildcard is rejected).
  cors:
    allowed_origins:
      - http://localhost:3000     # Next.js / Create React App
      - http://localhost:5173     # Vite
    allow_credentials: true

# The bootstrap admin credentials and the preview token are SECRETS, so they are
# env-only and never live in this file — see .env.example.
`

const starterEnvExample = `# Copy this file to .env and fill it in. These are secrets, so they are read from
# the environment, never from dcms.config.yaml. How you load .env is up to you
# (direnv, docker --env-file, your shell, etc.); DCMS reads process env vars.

# The first admin, seeded automatically the first time you run 'dcms dev' while
# the user table is empty. Use a real address and a strong password.
DCMS_ADMIN_EMAIL=you@example.com
DCMS_ADMIN_PASSWORD=change-me-to-something-strong

# Optional: a long random string that unlocks reading draft/unpublished content
# via the X-DCMS-Preview header. Leave unset to serve only published content.
# DCMS_PREVIEW_TOKEN=
`

const starterGitignore = `# DCMS local data — do not commit
dcms.db
dcms.db-shm
dcms.db-wal
dcms-media/
.env
`
