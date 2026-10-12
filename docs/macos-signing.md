# macOS signing and notarization (fork releases)

`fork-release.yml` builds the Apple Silicon app. When the six repository secrets
below are set, the build signs it with your Developer ID Application certificate,
notarizes it with Apple, staples the ticket, and **fails the release** unless
`codesign --verify --deep --strict`, `spctl --assess --type execute` and
`xcrun stapler validate` all pass. With none of the secrets set it still builds,
unsigned, and prints a warning. With only some of them set it fails.

Signed builds open without the Gatekeeper "damaged" error, and the in-app
updater works: it installs `latest-mac.yml` + the `.zip` through Squirrel.Mac,
which requires the new app's code signature to satisfy the running app's
designated requirement. Every release signed by the same Developer ID team
satisfies it.

> **First signed release.** Installed unsigned copies cannot auto-update to a
> signed one (the signatures do not match). Install the first signed release
> manually over the old app (download the zip, replace the app in
> `/Applications`). Releases after that update in place.

## 1. Enroll in the Apple Developer Program

1. Go to <https://developer.apple.com/programs/enroll/> and sign in with the
   Apple ID you want to own the account.
2. Enroll as an individual or organization (US$99/year). Organizations need a
   D-U-N-S number and can take days; individuals are usually approved within a day.
3. Note your **Team ID** (10 characters): developer.apple.com > Account >
   Membership details.

## 2. Create the Developer ID Application certificate

Only the Account Holder can create Developer ID certificates.

1. Open **Keychain Access** > menu *Keychain Access > Certificate Assistant >
   Request a Certificate From a Certificate Authority*.
2. Enter your email and a name, choose **Saved to disk**, and save
   `CertificateSigningRequest.certSigningRequest`. (A private key is created in
   your login keychain.)
3. At <https://developer.apple.com/account/resources/certificates/add> choose
   **Developer ID Application**, upload the CSR, and download the `.cer`.
4. Double-click the `.cer` to install it in the login keychain.
5. In Keychain Access > *My Certificates*, find
   `Developer ID Application: Your Name (TEAMID)`. Expand it, confirm the private
   key is under it, right-click the certificate > **Export...** > format `.p12`,
   and set an export password. Remember the password (this is `CSC_KEY_PASSWORD`).
6. The certificate's full name, exactly as shown, is `APPLE_SIGNING_IDENTITY`.
   You can also list it with `security find-identity -v -p codesigning`.

## 3. Create the App Store Connect API key (for notarization)

1. <https://appstoreconnect.apple.com/access/integrations/api> > **Team Keys** >
   **+** (request access to the API first if prompted).
2. Name it, choose access **Developer**, and generate it.
3. **Download the `.p8` immediately**; Apple lets you download it once.
4. Copy the **Key ID** (`APPLE_API_KEY_ID`) from the row and the **Issuer ID**
   (a UUID, `APPLE_API_ISSUER`) from the top of the page.

## 4. The six secrets

| Secret | Value |
| --- | --- |
| `CSC_LINK` | base64 of the exported `.p12` |
| `CSC_KEY_PASSWORD` | the `.p12` export password |
| `APPLE_SIGNING_IDENTITY` | `Developer ID Application: Your Name (TEAMID)` |
| `APPLE_API_KEY_BASE64` | base64 of `AuthKey_XXXXXXXXXX.p8` |
| `APPLE_API_KEY_ID` | the 10-character Key ID |
| `APPLE_API_ISSUER` | the Issuer ID UUID |

No keychain password secret is needed: the workflow's
`.github/actions/macos-signing-setup` imports the `.p12` into a temporary
keychain with a generated password (via `apple-actions/import-codesign-certs`).

### Set them with `gh`

Values are read from files or stdin, never from the command line, so they stay
out of shell history and process listings. Replace the paths.

```sh
REPO=biz-direct/agent-orchestrator

# base64 straight from the file into gh (no value on the command line)
base64 < ~/Downloads/DeveloperID.p12   | tr -d '\n' | gh secret set CSC_LINK             --repo "$REPO"
base64 < ~/Downloads/AuthKey_XXXXXXXXXX.p8 | tr -d '\n' | gh secret set APPLE_API_KEY_BASE64 --repo "$REPO"

# plain values: gh prompts for them (hidden input) when no stdin is piped
gh secret set CSC_KEY_PASSWORD     --repo "$REPO"
gh secret set APPLE_SIGNING_IDENTITY --repo "$REPO"
gh secret set APPLE_API_KEY_ID     --repo "$REPO"
gh secret set APPLE_API_ISSUER     --repo "$REPO"
```

Or run `scripts/setup-macos-signing-secrets.sh`, which prompts for the two file
paths and the four values (hidden input) and runs the commands above.

Afterwards, delete the `.p12` and `.p8` from disk or keep them in a password
manager, since Apple will not re-issue the `.p8`.

## 5. Cut a signed release

Bump `FORK_VERSION` on `main` (or run **Fork release** from the Actions tab).
In the mac build job you should see *Set up macOS signing*, the notarization wait
(typically a few minutes), and *Verify macOS signature, Gatekeeper and
notarization ticket* pass. The job summary shows the verification result.

Manual check of a downloaded release:

```sh
ditto -x -k agent-orchestrator-<version>-darwin-arm64.zip out/
codesign --verify --deep --strict --verbose=2 "out/Agent Orchestrator.app"
spctl --assess --type execute --verbose=4 "out/Agent Orchestrator.app"
xcrun stapler validate "out/Agent Orchestrator.app"
```

Use `ditto`, not `unzip`; `unzip` adds `._*` files that break the seal.

## Troubleshooting

- *Build fails: "partially configured"*: one of the six secrets is missing or empty.
- *"The specified item could not be found in the keychain"*: `CSC_LINK`
  was exported without the private key; re-export the `.p12` from *My Certificates*.
- *Notarization "Invalid"*: run `xcrun notarytool log <submission-id> --key ... `
  using the printed submission id; usually an unsigned nested binary or a
  missing hardened-runtime flag.
- *Updater says the signature does not match*: the installed app was signed
  by a different team or unsigned; install manually once (see above).
