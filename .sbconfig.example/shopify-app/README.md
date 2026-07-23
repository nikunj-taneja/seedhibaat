# Shopify app configuration

This placeholder shows where a private deployment keeps the Shopify
CLI-managed app configuration. Do not put a real app client ID, shop domain, or
callback hostname in this tracked example.

After copying `.sbconfig.example/` to the ignored `.sbconfig/`, link the
existing app:

```bash
shopify app config link \
  --path .sbconfig/shopify-app \
  --client-id "$SHOPIFY_CLIENT_ID"
```

Shopify CLI creates or overwrites `.sbconfig/shopify-app/shopify.app.toml`.
Merge the compliance subscription from the example TOML into that pulled
configuration, using the deployment's real HTTPS webhook URI. Review the
complete diff before running `shopify app deploy`.
