# Configuration sync

Paperboat synchronizes only home-relative paths selected by `.pbinclude`; `.pbignore`
can narrow that scope. Credentials, `.env` files, SSH material and Paperboat runtime
state are always excluded. Repository contents and Git history are ordinary plaintext
at the Git provider, so keep configuration secrets in ENV rather than tracked files.

In the interactive CLI, open **Diagnostics → Config sync** or **Configuration → Config sync**.
Choose **Enable sync** to connect GitHub in your browser, select authorized repositories,
choose pull/push direction, and confirm the plaintext Git storage warning. A remote or
headless terminal can display a one-use browser link instead. The menu configures the
machine running that CLI, including when opened inside a remote Paperboat terminal.

The same menu provides status, repository changes, reviewed update approval, worker
repair, and disabling sync. Disabling removes the assignment and stops the worker;
existing repository content and applied files remain. Windows may ask for its normal
administrator approval when installing or removing the worker. Cancellation of browser
setup cancels the pending connection; a completed provider connection remains available.

For scripted setup, connect an authorized provider repository and configure a machine:

```text
pb config assign <repository> <machine> --mode pull-only
pb config assign <pull-repository> <machine> --mode bidirectional \
  --push-repository <push-repository>
```

For a machine assignment, the first command shows the plaintext Git storage warning and
a confirmation command. Run that displayed command within five minutes to apply it.

Pull and push targets are independent. Paperboat uses your connected provider credential
for each operation; it does not grant repository access or bypass read-only permissions,
branch protection, divergent histories, or non-fast-forward rejection. Missing or revoked
provider credentials stop synchronization. Paperboat never force-pushes automatically.

Remote changes are fetched and reported as `review_required` before managed home files are
changed. Review the shown revision and run `pb config approve <machine>` to bind approval
to that exact head. A changed head makes the approval stale. `--automatic-updates` opts the
assignment into later updates within the already selected manifest scope. Chezmoi
templates and command-bearing attributes (`run`, `modify`, `external`, and related forms) remain unsupported and rejected;
automatic updates therefore cannot broaden into command execution.

Team owners and admins can set a default pull repository with
`pb config team-default-set <team> <repository>`. A member must connect that same repository
with their own provider account and explicitly run `pb config team-default-adopt <team>`.
Only one team default can be adopted at a time; adopting another explicitly replaces it.
A personal machine assignment wins and a changed team default remains pending until the
member adopts its new version. Unadoption or PB membership removal stops inheritance but
does not change external Git access or redirect a personal push target.

Application uses a private recovery journal. The runtime retains one bounded previous
managed-file image for rollback and restores an interrupted apply before continuing.
Conflicts leave affected paths unchanged while unrelated managed paths continue. Git
pushes are normal atomic pushes when supported by the provider; Paperboat does not promise
simultaneous visibility across arbitrary home-directory files to other local processes.
