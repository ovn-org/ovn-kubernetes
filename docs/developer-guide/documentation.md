Documentation is an important step in the SDLC process. Unless there are
proper docs to tell end users about the code you contributed; there will
be less visibility and understanding of your code and feature. It will also
become hard to maintain such code.

## Writing Documentation

All OVN-Kubernetes docs are kept under the `docs/` folder in the main path.
There are specific folders such as `features` and `developer-guide` where
you can add your commit against. These simple `.md` files are referred from
the navigation tile in `mkdocs.yml` file which is what is used to
build our [website](https://ovn-kubernetes.io/).

As a developer and contributor to this project; it is recommended that you
include a docs commit in your PR whenever possible and relevant. Some examples
where we mandate docs include:

* **Enhancement Proposals**: If you are planning to do a new feature, then start
with an enhancement proposal a.k.a OKEP so that maintainers and other reviewers
can get an understanding of what your goals are. See [here](../okeps/okep-4368-template.md)
for more details and open a commit adding it to our `docs/okeps` folder.

* **Architecture and Topology docs**: Did you change the architecture or topology?
If so please write docs reflecting your design changes and update any existing docs
so that they remain relevant. Open a commit adding it to our `docs/design` folder.

* **Feature Docs**: If your enhancement proposal has merged; next step is to
implement that feature. As part of the main implementation PR we mandate adding a
feature documentation commit. See [feature documentation template](../features/template.md)
for how a feature documentation should be done. Open a commit adding it to our
`docs/features` folder.

* **Contributor Docs**: If your changes include code cleanup or refactoring
that is not user facing but internal; example: "Adding DBIndexes to AddressSets" write up a
developer guide for contributors asking them to follow that new code structure process while
adding new AddressSets. Open a commit adding it to our `docs/developer-guide` folder.

* **Bug Fixes**: If there was a difficult or critical bug fix that warrants a doc; please
feel free to write it. If you are unsure where this should be placed; reach out to the maintainers.

* **Blog Posts**: Are you an end-user of OVN-Kubernetes? Is there something you wish to share with
the community about your awesome use cases and how you used our CNI to solve your problems? We
welcome blog post contributions from all! See [OVN-Kubernetes blog](https://ovn-kubernetes.io/blog/) for details.
Open a commit adding your post to `docs/blog/posts/`. Each post must include
YAML front matter with a `date` and `authors` list, for example:
```yaml
---
date: 2026-03-12
authors:
  - githubhandle
---
```
The author handle must have a matching entry in `docs/blog/.authors.yml` with
your name, a short description, and a GitHub avatar URL. See existing entries
for reference.

* **Performance Enhancements**: We love performance enhancements! Did you write a cool patch
to reduce the time it takes for iptables to sync up on startup? Think about writing a good blog post
around this! Open a commit adding it to our `docs/blog/posts/` folder.

* **API Reference**: Did you introduce a new CRD? OR Did you add a watcher a new CRD? Include API
Reference documentation changes to `docs/api-reference` folder. 
```bash
make -C docs/ generate-api-reference
```
See [here](../api-reference/introduction.md) for more details.

## Website Guide

We are utilizing [GitHub Pages](https://docs.github.com/en/pages/quickstart) to host the ovn-kubernetes.io website. The website's
content is composed in Markdown and stored within the `docs/` directory at the root
of our repository. For static site generation, we employ the [MkDocs](https://www.mkdocs.org/user-guide/writing-your-docs/) framework,
managed by the `mkdocs.yml` configuration file located next to the `docs/` folder.
Additionally, we use the [Material](https://squidfunk.github.io/mkdocs-material/setup/) framework on top of MkDocs, which enhances our
site with advanced features like customizable navigation, color schemes, and site
search capabilities.

The published site is **versioned** with [mike](https://github.com/jimporter/mike).
Each `release-*` branch and `master` appears in a version dropdown on the docs site.
Deploy logic lives in
[`hack/build-docs.sh`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/hack/build-docs.sh)
and is invoked via `make -C docs build-docs` (see
[`docs/Makefile`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/docs/Makefile)).
The
[Publish Versioned Docs](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/.github/workflows/docs-versioning.yml)
workflow runs that target with `PUSH=1` and updates the `gh-pages` branch.

The [docs.yml](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/.github/workflows/docs.yml)
workflow keeps a `test-deploy` job so pull requests still validate that docs build
cleanly. It does not publish the live site; publishing is owned by the versioning
workflow so the two do not overwrite each other.

### Versioned docs: how publishing works

The versioning workflow runs on pushes to `master` or any `release-*` branch when
docs-related paths change (`docs/**`, `mkdocs.yml`, the workflow file,
`hack/build-docs.sh`, `hack/docs-site-404.html`, or `requirements.txt`). By default
each run rebuilds **all** versions so Latest/Stable/Legacy labels and the version
dropdown stay consistent.

- The nav structure (`nav:` in `mkdocs.yml`) is **per-branch**. A new page added
  on `master` does not appear in older release versions (and will not break them).
  To ship a page in an older release, backport the markdown **and** update that
  branch's `mkdocs.yml` nav.
- Visual styling is shared: the build copies `docs/stylesheets/extra.css` from
  `master` onto every version at build time. Make CSS changes on `master` only.
  Do not edit `extra.css` on release branches — the file is overwritten, and
  `hack/build-docs.sh` hard-fails if it has local modifications on a `release-*`
  branch.
- When a new `release-X.Y` branch is created, the workflow discovers it
  (via the `create` event and/or a docs push) and the dropdown labels shift:
  - highest `release-*` → `<ver> (Latest)` (also the mike `latest` alias)
  - second-highest → `<ver> (Stable)`
  - older releases → `<ver> (Legacy)`
  - `master` → `Master (Dev)`
  Example today: `1.3 (Latest)`, `1.2 (Stable)`, `1.1 (Legacy)`, `1.0 (Legacy)`.
  After `release-1.4` appears: `1.4 (Latest)`, `1.3 (Stable)`, and older stay
  `(Legacy)`. No workflow edits are required.
- Every branch that is deployed must have a root `mkdocs.yml`. The build injects
  `extra.version.provider: mike` if missing, but the file itself must exist.
- Do not manually push to or develop on `gh-pages`. The versioning workflow owns it.
- Prefer `make -C docs build-docs` over ad-hoc `mike` commands so the same
  validations run locally and in CI.

### Linking between docs pages (use relative paths)

Mike publishes each docs version under its own URL prefix — for example
`/master/...`, `/1.3/...`, or the `latest` alias. A page that used to live at
`https://ovn-kubernetes.io/design/architecture/` is now served at
`https://ovn-kubernetes.io/master/design/architecture/` (or under the release
you selected in the version dropdown).

**Do not** link to docs pages with unversioned absolute site URLs such as
`https://ovn-kubernetes.io/design/architecture/` or
`https://ovn-kubernetes.io/installation/...`. Those paths no longer exist at
the site root after versioning, so they 404. The same problem affects any
hard-coded `https://ovn-kubernetes.io/<page>/` link that omits the version
segment.

**Going forward, link with relative Markdown paths** so MkDocs resolves the
target within the version currently being viewed:

```markdown
<!-- Good: stays inside the current mike version -->
See the [Architecture](../design/architecture.md) page.

<!-- Bad: 404s on the versioned site -->
See the [Architecture](https://ovn-kubernetes.io/design/architecture/) page.
```

Relative links also keep working when someone switches the version dropdown
(for example from Master to a `release-*` docs tree), because the browser stays
under that version’s prefix.

A bare homepage link to `https://ovn-kubernetes.io/` is fine (the root still
redirects into the default version). External sites, GitHub URLs, and other
non-docs destinations should keep using absolute URLs as usual.

**Exception: root files symlinked into `docs/governance/`.** Files such as
`CONTRIBUTING.md`, `GOVERNANCE.md`, and `MEETINGS.md` live at the repository
root and are symlinked under `docs/governance/` for the website. A relative
path that is correct from `docs/governance/` (for example
`../installation/launching-ovn-kubernetes-on-kind.md`) breaks when the same
file is viewed on GitHub at the repo root. For those shared files, use a
versioned absolute docs URL instead, for example
`https://ovn-kubernetes.io/master/installation/launching-ovn-kubernetes-on-kind/`,
so the link works both on GitHub and on the published site.

When you touch docs links in a PR, prefer relative paths under `docs/` and
avoid introducing new unversioned `ovn-kubernetes.io/<page>/` URLs.

Repo files outside `docs/` that must deep-link into the published site (for
example `README.md`) should use a versioned absolute URL under `/master/…`,
not an unversioned path.

### Root `404.html` (pre-versioning deep links)

GitHub Pages serves a site-root `404.html` when a URL is missing. Mike only
publishes version directories (`master/`, `1.3/`, …) and does not install a root
404, so old bookmarks such as `https://ovn-kubernetes.io/design/architecture/`
would otherwise hit GitHub’s generic 404.

`hack/build-docs.sh` copies `hack/docs-site-404.html` to `404.html` on the
`gh-pages` branch after each deploy. That page redirects unversioned paths to
the same path under `/master/…`. Paths that already include a version or alias
(`master`, `latest`, or `N.N`) and are still missing go to that version’s home.
If the request is already at a version root that does not exist (so a second
redirect would loop on itself), the page falls back to `/master/` (or `/` when
the prefix is already `master`).

Do not hand-edit `404.html` on `gh-pages`; change `hack/docs-site-404.html` on
`master` and let the versioning workflow republish it.

**Redirect validation (`safeRedirect` helper):** All `location.replace()` calls
in the 404 page go through a `safeRedirect()` function that enforces two
invariants before redirecting:

1. **Leading-slash normalization** — multiple leading slashes are collapsed to
   one (`//…` → `/…`) so the browser never interprets the URL as a
   protocol-relative redirect (e.g. `//evil.com`).
2. **Origin check** — the URL is parsed with the `URL` API against the current
   origin; if the resolved origin differs the redirect falls back to `/master/`.

Every redirect target is an origin-relative path by construction, so these
checks are defense-in-depth rather than a fix for an exploitable bug.

Shell coverage for the install helper (local/remote `gh-pages`, unchanged vs
changed template, `PUSH=false`/`true`, push failure) and for the clean-worktree
preflight lives in `hack/test-install-gh-pages-404.sh`
(`make -C docs test-install-gh-pages-404`).

### Manual rebuild (workflow_dispatch)

Anyone with write access can trigger a rebuild from the **Actions** tab
(Publish Versioned Docs). The `releases` input defaults to `all`. Set it to a
comma-separated list of existing branches (for example `release-1.1` or
`release-1.1,master`) to rebuild only those. Unknown branch names fail the job.

### What triggers a rebuild (and what does not)

| Event | Rebuilds docs site? |
|-------|---------------------|
| Push to `master` / `release-*` changing `docs/**`, `mkdocs.yml`, the versioning workflow, `hack/build-docs.sh`, `hack/docs-site-404.html`, or `requirements.txt` | Yes (all versions) |
| Creating a new `release-*` branch | Yes (`create` event; labels recompute) |
| Manual **workflow_dispatch** | Yes (`all` or the branches you list) |
| Go-only / non-docs commits on `master` or `release-*` | No (by design) |

### After this lands on master (one-time + first-run checks)

Upstream currently has GitHub Pages enabled but **no `gh-pages` branch** — the
old site was published with the Actions `deploy-pages` flow. Mike needs branch
publishing. A repo admin must do this once after the first successful
**Publish Versioned Docs** run (or right before it):

1. Open
   [Settings → Pages](https://github.com/ovn-kubernetes/ovn-kubernetes/settings/pages)
   on `ovn-kubernetes/ovn-kubernetes`.
2. Under **Build and deployment → Source**, choose **Deploy from a branch**.
3. Branch: **`gh-pages`** / folder: **`/` (root)**. Save.
4. Keep the custom domain (`ovn-kubernetes.io`) as already configured.

Then verify the first merge deploy:

1. Actions → **Publish Versioned Docs** is green for the merge commit.
2. The `gh-pages` branch exists and contains `versions.json` plus `master/`,
   `1.3/`, `1.2/`, …
3. Locally: `git fetch origin gh-pages && mike list` shows titles like
   `Master (Dev)`, `1.3 (Latest)`, `1.2 (Stable)`, `1.1 (Legacy)`, `1.0 (Legacy)`.
4. On the live site, the version dropdown switches and each version’s nav/content
   matches that release branch.

If Pages is still set to **GitHub Actions**, the workflow can succeed while the
public site does not update — fix the Source setting above.

## Markdown extensions and plugins

The docs site enables several MkDocs plugins and pymdownx Markdown extensions
beyond basic Markdown. Use them to make documentation clearer and more
interactive.

### Tabbed content (`pymdownx.tabbed`)

Show alternatives (e.g. `kubectl` vs `oc`, IPv4 vs IPv6) side-by-side:

```markdown
=== "kubectl"

    ```bash
    kubectl get pods -A
    ```

=== "oc"

    ```bash
    oc get pods -A
    ```
```

### Task lists (`pymdownx.tasklist`)

Render GitHub-style checklists:

```markdown
- [x] Create the CRD types
- [x] Run `make codegen`
- [ ] Add feature documentation
```

### Keyboard shortcuts (`pymdownx.keys`)

Render keyboard keys with the `++` syntax:

```markdown
Press ++ctrl+c++ to copy, ++ctrl+v++ to paste.
```

### Magic links (`pymdownx.magiclink`)

GitHub references are auto-linked. No Markdown link syntax needed:

```markdown
See #1234 for details.          <!-- links to issue/PR 1234 -->
Fixed by @username in abc123.   <!-- links to user and commit -->
```

The config sets `user: ovn-kubernetes` and `repo: ovn-kubernetes` so
bare `#1234` references resolve to this repository.

### Text highlighting (`pymdownx.mark`)

Highlight important text with double equals:

```markdown
This is ==highlighted text== in a sentence.
```

### Macros (`mkdocs-macros-plugin`) and Jinja2 escaping

The macros plugin enables Jinja2 templating in Markdown. It processes
**all** content, including fenced code blocks.

{% raw %}
**Escaping rule:** If your docs contain literal `{{ }}` syntax (Go templates,
Helm charts, Docker inspect commands, etc.), you **must** escape it so the
macros plugin does not try to evaluate it:

```markdown
<!-- Raw Go template (will break the build): -->
The default output is `pkg/testing/mocks/{{.PackagePath}}/`

<!-- Escaped (correct): -->
The default output is `pkg/testing/mocks/{{ '{{' }}.PackagePath{{ '}}' }}/`
```

The `{{ '{{' }}` expression outputs a literal `{{` and `{{ '}}' }}` outputs
a literal `}}`. This applies anywhere `{{ }}` appears in docs — inline code,
fenced code blocks, or prose.
{% endraw %}

### Page revision dates (`git-revision-date-localized`)

Each page automatically shows a "Last updated" date at the bottom, pulled
from git history. No author action is needed. The plugin falls back to the
build date when full git history is unavailable (e.g. CI shallow clones).

### Redirects (`mkdocs-redirects`)

When a page is moved or renamed, add an entry to the `redirect_maps` in
`mkdocs.yml` so old URLs do not 404:

```yaml
plugins:
  - redirects:
      redirect_maps:
        'old/path.md': 'new/path.md'
```

### HTML minification (`mkdocs-minify-plugin`)

The built site HTML is automatically minified for faster page loads. No
author action is needed.

## How to test your documentation changes?

### Option 1) Build and view docs with a PR

Pushing docs changes to the ovn-kubernetes/ovn-kubernetes project as a pull request will
run the job name "[Test Docs Build](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/.github/workflows/docs.yml)" which has a step to save the
docs artifacts. You can download those as a .zip file, extract and view them locally.

### Option 2) Build and serve a single version locally

In order to test changes locally to either mkdocs.yml or to files under docs/ folder,
please follow the instructions below.

## Clone the repository
```text
# git clone https://github.com/ovn-kubernetes/ovn-kubernetes
Cloning into 'ovn-kubernetes'...
remote: Enumerating objects: 84258, done.
remote: Counting objects: 100% (1546/1546), done.
remote: Compressing objects: 100% (686/686), done.
remote: Total 84258 (delta 809), reused 1208 (delta 663), pack-reused 82712
Receiving objects: 100% (84258/84258), 56.39 MiB | 28.74 MiB/s, done.
Resolving deltas: 100% (55993/55993), done.

# cd ovn-kubernetes
```

## Create a Python virtual environment
```python
#  python -m venv venv
```

## Activate the virtual environment
```text
# source venv/bin/activate
```

## Install all the required python packages to render the website
```text
(venv) # pip install -r requirements.txt 
Collecting Click
  Using cached click-8.1.7-py3-none-any.whl (97 kB)
Collecting htmlmin

<output snipped for brevity>
```
## Run the website locally using
```text
(venv) # mkdocs serve
INFO    -  Building documentation...
INFO    -  Cleaning site directory

<output snipped for brevity>

INFO    -  Documentation built in 1.52 seconds
INFO    -  [17:05:21] Watching paths for changes: 'docs', 'mkdocs.yml'
INFO    -  [17:05:21] Serving on http://127.0.0.1:8000/ovn-kubernetes/
```
Now you can browse the website on your browser using the above URL.

As you make changes and save the files, the mkdocs server notices that and re-builds the website.
It also spews out any WARNINGS or ERRORS with respect to the changes that you have just made. If
the changes look good, then go ahead and submit the PR.

### Option 3) Preview versioned docs locally with mike

Install dependencies once:

```bash
pip install -r requirements.txt
```

**Preview unmerged work on your feature branch** (CSS, `mkdocs.yml`, dropdown UI).
`make -C docs build-docs` is the wrong tool for this — it checks out remote
`origin/master` / `origin/release-*` and will not show your local PR changes.

```bash
# On your feature branch:
mike deploy --title "Master (Dev)" master
mike set-default master
mike serve
```

Open `http://localhost:8000`. For a single-version preview without mike, use
`mkdocs serve` as in Option 2.

**Mimic what CI publishes** (content from remote branches only):

```bash
make -C docs build-docs                 # all origin/release-* + origin/master
make -C docs build-docs RELEASES=master # only origin/master
mike serve
```

Push the local `gh-pages` branch only if you intend to update the remote
(CI already does this with `PUSH=1`):

```bash
make -C docs build-docs PUSH=1
```

`hack/build-docs.sh` refuses to run while checked out on `gh-pages`, refuses
to continue if `extra.css` is dirty on a `release-*` branch, and refuses to
run at all when any tracked file has local modifications (mike and the root
404 install use `git checkout -f`, which would discard those changes). There
is no interactive override.
