// ESLint with typescript-eslint's strictest type-aware rule sets.

import { defineConfig } from "eslint/config";
import tseslint from "typescript-eslint";

export default defineConfig(
    // Output of `npm run build`.
    { ignores: ["dist/"] },
    tseslint.configs.strictTypeChecked,
    tseslint.configs.stylisticTypeChecked,
    {
        languageOptions: {
            parserOptions: {
                // The config itself is JavaScript, outside tsconfig.json.
                projectService: { allowDefaultProject: ["eslint.config.js"] },
                tsconfigRootDir: import.meta.dirname,
            },
        },
    },
);
