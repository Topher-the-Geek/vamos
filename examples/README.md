# Examples

Each example includes a build diagram and instructions for development and running its images.
The [tutorial](../docs/tutorial.md) walks through the TypeScript UI example.

- [TypeScript UI](typescript-ui/README.md): TypeScript, pnpm, Vite, nginx.

  A simple build that installs dependencies,
  bundles a website into a task's fileset, and adds it to an nginx image.

- [TypeScript MCP](typescript-mcp/README.md): TypeScript, npm.

  Separate build and runtime dependencies,
  with a task compiling TypeScript and a runtime image
  containing the compiled server and production dependencies.

- [FastMCP](fastmcp/README.md): Python, uv.

  A Python service built from locked dependencies and source,
  with dependency installation cached separately from application changes.

- [Go CLI](golang-cli/README.md): Go, cross-compilation, scratch image.

  Native cross-compilation for amd64 and arm64 without emulation,
  minimal `scratch` images, a platform index, and debug/release build parameters.

- [Strawberry React](strawberry-react/README.md): Python, TypeScript, monorepo,
  cross package dependencies, GraphQL code generation.

  A monorepo with dependencies across packages and ecosystems.
  The Python backend exports a GraphQL schema used to generate types for the React/TypeScript UI.
