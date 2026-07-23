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

The example template and workflow are disabled documentation. Never submit the
template or activate the workflow without completing the normal dry-run,
review, exact-template approval, audience preview, and activation gates.
