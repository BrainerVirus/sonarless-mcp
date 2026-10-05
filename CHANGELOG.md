# Changelog

All notable changes are generated from [Conventional Commits](https://www.conventionalcommits.org/) by semantic-release; GitHub Releases carry the same notes.

## [0.5.1](https://github.com/BrainerVirus/sonarless-mcp/compare/v0.5.0...v0.5.1) (2026-10-05)

### Bug Fixes

* **clients:** keep symlinked configs and detect inline Codex entries ([8aaf0a6](https://github.com/BrainerVirus/sonarless-mcp/commit/8aaf0a6f7cea3e0f6f0f700f0085d8265885f1f4))
* **config:** handle BOMs and comments after quoted values ([1462a3e](https://github.com/BrainerVirus/sonarless-mcp/commit/1462a3e9ecf9659ff3e62cfd79c1ccb78f7f832e))
* **install:** Windows-safe updates, PATH via registry, no temp leaks ([ee72815](https://github.com/BrainerVirus/sonarless-mcp/commit/ee72815668489020a2b4a588a8d9def5efa3e07b))
* **mcp:** degrade instead of failing, and never leave a request unanswered ([d3db59f](https://github.com/BrainerVirus/sonarless-mcp/commit/d3db59fa2b5567e8f745f6eec344de28fcc20f30))
* **remote:** pick free ports and tell network errors from rejected tokens ([641a819](https://github.com/BrainerVirus/sonarless-mcp/commit/641a819cc4fd29d3fc8054df2a74984cb21f9b10))
* **scan:** keep files user-owned and explain busy ports ([356d1f7](https://github.com/BrainerVirus/sonarless-mcp/commit/356d1f7988b62957b787769a52ade513ae6c27ec))
* **sync:** apply what it can and report the rest ([534654c](https://github.com/BrainerVirus/sonarless-mcp/commit/534654c22f9c6558b53a85a0b452fd340126d18d))

## [0.5.0](https://github.com/BrainerVirus/sonarless-mcp/compare/v0.4.0...v0.5.0) (2026-10-05)

### Features

* **agents:** expose sonarless tools over MCP and install an agent skill ([48b1c25](https://github.com/BrainerVirus/sonarless-mcp/commit/48b1c2502d541fc3238f4219e66a53fd140122fd))

## [0.4.0](https://github.com/BrainerVirus/sonarless-mcp/compare/v0.3.6...v0.4.0) (2026-10-05)

### Features

* **remote:** sync a project's quality gate, rule sets and new code from a remote ([3add001](https://github.com/BrainerVirus/sonarless-mcp/commit/3add001e84c76547153c04b9dc50d4e48c2106c6))

## [0.3.6](https://github.com/BrainerVirus/sonarless-mcp/compare/v0.3.5...v0.3.6) (2026-10-05)

### Bug Fixes

* **remote:** run remote MCP containers on the local container's exact image ([dfc8ac8](https://github.com/BrainerVirus/sonarless-mcp/commit/dfc8ac803b16be2f29379c10bd00936fd4552648))

## [0.3.5](https://github.com/BrainerVirus/sonarless-mcp/compare/v0.3.4...v0.3.5) (2026-10-05)

### Bug Fixes

* **remote:** refuse analysis tokens with a clear explanation ([4bc5f13](https://github.com/BrainerVirus/sonarless-mcp/commit/4bc5f13857460157ec5605268d9540aabf8cda0c))

## [0.3.4](https://github.com/BrainerVirus/sonarless-mcp/compare/v0.3.3...v0.3.4) (2026-10-05)

### Bug Fixes

* **install:** name the terminal and tailor the refresh hint for it ([eafa40d](https://github.com/BrainerVirus/sonarless-mcp/commit/eafa40d7b142386c329e8213085181000baeb1c8))

## [0.3.3](https://github.com/BrainerVirus/sonarless-mcp/compare/v0.3.2...v0.3.3) (2026-10-05)

### Bug Fixes

* **install:** give the right refresh hint for the terminal in use ([df1ca38](https://github.com/BrainerVirus/sonarless-mcp/commit/df1ca3871773e1de18156e4b03a18da63555fc71))

## [0.3.2](https://github.com/BrainerVirus/sonarless-mcp/compare/v0.3.1...v0.3.2) (2026-10-05)

### Bug Fixes

* **install:** set up PATH for every shell on the system ([057df1e](https://github.com/BrainerVirus/sonarless-mcp/commit/057df1e03834ef2184b2e07d7c532e3a3f197e1e))

## [0.3.1](https://github.com/BrainerVirus/sonarless-mcp/compare/v0.3.0...v0.3.1) (2026-10-05)

### Bug Fixes

* **install:** put the binary on PATH and say how to refresh the shell ([38d2bc0](https://github.com/BrainerVirus/sonarless-mcp/commit/38d2bc0c72feb98312b3ab6b5deaa17fed24fa62))

## [0.3.0](https://github.com/BrainerVirus/sonarless-mcp/compare/v0.2.1...v0.3.0) (2026-10-05)

### Features

* **install:** one cross-platform installer entry point ([1ae8eb9](https://github.com/BrainerVirus/sonarless-mcp/commit/1ae8eb95a40c9d89009aae36aa85c1853a25fcc4))
* **remote:** add `remote update` to change a remote's url, branch or token ([c6ffce2](https://github.com/BrainerVirus/sonarless-mcp/commit/c6ffce23574078bddce00a562571aec1ad269379))

## [0.2.1](https://github.com/BrainerVirus/sonarless-mcp/compare/v0.2.0...v0.2.1) (2026-10-05)

### Bug Fixes

* **remote:** make the token prompt cancellable and validate tokens ([33326cc](https://github.com/BrainerVirus/sonarless-mcp/commit/33326ccddd72adf338bd7d87f69151caa09cab66))

## [0.2.0](https://github.com/BrainerVirus/sonarless-mcp/compare/v0.1.0...v0.2.0) (2026-10-05)

### Features

* **remote:** query remote SonarQube servers next to the local one ([490a5a7](https://github.com/BrainerVirus/sonarless-mcp/commit/490a5a77ba6000dffefaa8a65a723feef109ee77))

## [0.1.0](https://github.com/BrainerVirus/sonarless-mcp/compare/v0.0.0...v0.1.0) (2026-10-05)

### Features

* **update:** self-update on start and refresh the shared MCP image ([592123c](https://github.com/BrainerVirus/sonarless-mcp/commit/592123cc4667d4b47acae8d294e38284148cdf79))

### Bug Fixes

* **setup:** never overwrite another sonarqube MCP server ([e1f4aa3](https://github.com/BrainerVirus/sonarless-mcp/commit/e1f4aa30acfde4d3490a6d709989c05167b2e180))
