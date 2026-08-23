// Types for the Vite import suffixes the two cross-language conformance suites
// use to reach the Go-generated goldens in ../../render-golden/. Those files sit
// at the REPO root, outside this npm package, and used to be read with node:fs —
// which forced a DOM test into a Node environment. As Vite imports they are
// served to the browser instead, so the tests run where the renderer does.
//
// Declared here rather than by adding "vite/client" to tsconfig.test.json's
// `types`: vite is a transitive dependency of vitest, not a direct one, so
// naming it there would make this package depend on a package it does not
// declare. Only the two suffixes actually used are declared, so a mistyped
// `?inline` import must fail to compile rather than silently resolve — and
// ?inline in particular does NOT work on a .bin, which is not in Vite's default
// assetsInclude.
//
// This lives under test-helpers/ because package.json's `files` excludes that
// directory: an ambient `*?raw` module declaration must not be published into a
// consumer's type space.

/** `?raw` yields the file's contents as a UTF-8 string. */
declare module "*?raw" {
  const content: string;
  export default content;
}

/** `?url` yields a URL the browser can fetch, which is how a BINARY golden is
 *  read without a lossy string round-trip. */
declare module "*?url" {
  const url: string;
  export default url;
}
