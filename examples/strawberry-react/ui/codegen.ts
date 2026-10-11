import type { CodegenConfig } from '@graphql-codegen/cli';

const config: CodegenConfig = {
  overwrite: true,
  schema: "./schema.graphql",
  documents: "src/**/*queries.ts",
  generates: {
    "src/gql/types.ts": {
      plugins: [
        "typescript",
        "typescript-operations"
      ]
    }
  }
};

export default config;
