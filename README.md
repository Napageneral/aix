# AIX Family

This directory is the AIX app family container. The installable package unit is the manifest root, not the family root.

## Package roots
- `app/` -> package id `aix`

## Validate
```bash
bash packages/scripts/validate-package-roots.sh
```

## Release
```bash
nex package release packages/apps/aix/app
```
