# Public template media

Place only public JPEG or PNG template-header assets in the corresponding
ignored `.sbconfig/assets/` directory. Meta must be able to download each
configured `header_image_url` over HTTPS when a message is sent.

Deploy these files with:

```bash
SEEDHIBAAT_MEDIA_SOURCE=.sbconfig/assets tools/deploy.sh
```

Never put tokens, customer uploads, private files, or real deployment assets
in this tracked example directory.
