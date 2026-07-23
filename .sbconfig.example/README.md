# Private deployment bundle example

This tracked directory contains placeholders only. It teaches operators and
coding agents how a real SeedhiBaat deployment is organized.

Create an ignored private copy before configuring a business:

```bash
cp -R .sbconfig.example .sbconfig
chmod -R go-rwx .sbconfig
git check-ignore .sbconfig/config.yaml
```

Use the private copy for provider asset IDs, domains, template definitions,
workflows, and migration notes. Keep tokens, app secrets, signing keys,
verification tokens, verification codes, PINs, and customer data out of both
directories; those belong in an owner-only `.env` or server environment file.

The `shopify-app/` directory is a placeholder for Shopify CLI-managed app
configuration. In the ignored private copy, link an existing app with:

```bash
shopify app config link \
  --path .sbconfig/shopify-app \
  --client-id "$SHOPIFY_CLIENT_ID"
```

The link operation overwrites `shopify.app.toml` with the app's current Dev
Dashboard configuration. Review the resulting file before adding compliance
webhooks or running `shopify app deploy`; deployment changes external Shopify
app state.

The example template and workflow are disabled documentation. Never submit the
template or activate the workflow without completing the normal dry-run,
review, exact-template approval, audience preview, and activation gates.
