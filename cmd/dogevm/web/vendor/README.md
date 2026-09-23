# Vendored libraries

Copied from the npm tarballs, whose sha512 integrity matched the registry:

| Package | Version | Files | Integrity |
|---|---|---|---|
| @noble/secp256k1 | 2.3.0 | `index.js` | `sha512-0TQed2gcBbIrh7Ccyw+y/uZQvbJwm7Ao4scBUxqpBCcsOlZG0O4KGfjtNAy/li4W8n1xt3dxrwJ0beZ2h2G6Kw==` |
| qrcode-generator | 2.0.4 | `dist/qrcode.mjs` | `sha512-mZSiP6RnbHl4xL2Ap5HfkjLnmxfKcPWpWe/c+5XxCuetEenqmNFf1FH/ftXPCtFG5/TDobjsjz6sSNL0Sr8Z9g==` |
| @noble/hashes | 1.8.0 | `esm/{sha2,legacy,_md,_u64,utils,crypto}.js` | `sha512-jCs9ldd7NwzpgXDIf6P3+NrHh9/sD6CQdxHyjQI+h/6rDNo88ypBxxz45UDuZHz9r3tNz7N/VInSVoVdtXEI4A==` |

One change: `noble-hashes-1.8.0/utils.js` imports `./crypto.js` instead of the bare specifier `@noble/hashes/crypto`, so it loads in a browser without a bundler. All are MIT licensed (see each LICENSE, and the header of qrcode.mjs).

## Assets

- `../dogecoin.svg` is `share/pixmaps/dogecoin256.svg` from [dogecoin/dogecoin](https://github.com/dogecoin/dogecoin), used for the favicon, the masthead and the social preview image.
