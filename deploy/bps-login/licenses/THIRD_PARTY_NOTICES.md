# Component provenance and third-party notices

This distribution retains original copyright statements and module identities. It does not replace the licenses of its components with a single distribution-wide license.

| Component | Source / notice |
|---|---|
| CPA core | `github.com/router-for-me/CLIProxyAPI/v8`; original MIT license in `cpa-core/LICENSE` and `licenses/CPA-MIT-LICENSE` |
| Basis Points plugin | `github.com/jaxson-wang/cpa-plugin-oai-basispoints`; original module identity and source copyright statements retained; no standalone LICENSE was present in the supplied fork |
| Login runtime | Original package metadata identifies ISC; retained in `login-runtime/package.json` |
| node-tls-client 2.1.0 | Locked npm dependency; original notice in `licenses/npm/node-tls-client/` |
| tough-cookie 4.1.4 and transitive npm packages | Locked dependency metadata and original notices in `licenses/npm/` |
| TLS native library | [bogdanfinn/tls-client](https://github.com/bogdanfinn/tls-client), supplied Linux x64 asset; upstream notice in `licenses/tls-client/LICENSE` |
| Go dependencies | Versions in each component's `go.mod` / `go.sum`; available original notices collected in `licenses/go/` |
| Provider SDK and protocol assets | Kept byte-for-byte as required by the existing worker's integrity checks; upstream identifiers and source notices are retained |

The TLS native library's upstream notice includes the acknowledgement: “This product includes software developed by the &lt;organization&gt;.” The full upstream conditions are retained with the library.

Local machine paths, private deployment domains, accounts, runtime secrets and personal data are excluded separately from these public component identities and notices.
