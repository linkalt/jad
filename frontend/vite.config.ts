import fs from 'node:fs';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {defineConfig, type Plugin} from 'vite';
import react from '@vitejs/plugin-react';
import stylex from '@stylexjs/unplugin';
import appConfig from './src/config.json' with {type: 'json'};

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const schemesDir = path.resolve(__dirname, '../sample/schemes');
const schemesModuleId = 'virtual:catalog-schemes';

// The editor validates against the repo's JSON schemes. They are read with fs, not
// imported, because Vite looks up the nearest package.json of every imported file
// and a broken one above the repo would fail the build.
function catalogSchemes(): Plugin {
  return {
    name: 'catalog-schemes',
    resolveId: id => (id === schemesModuleId ? `\0${schemesModuleId}` : undefined),
    load(id) {
      if (id !== `\0${schemesModuleId}`) return undefined;
      const schemes: Record<string, unknown> = {};
      for (const file of fs.readdirSync(schemesDir).filter(name => name.endsWith('.json')).sort()) {
        const filePath = path.join(schemesDir, file);
        this.addWatchFile(filePath);
        schemes[file] = JSON.parse(fs.readFileSync(filePath, 'utf8'));
      }
      return `export default ${JSON.stringify(schemes)};`;
    },
  };
}

const lightningcssTargets = {
  chrome: appConfig.browser_targets.chrome_major << 16,
  firefox: appConfig.browser_targets.firefox_major << 16,
  safari: (appConfig.browser_targets.safari_major << 16) | (appConfig.browser_targets.safari_minor << 8),
};

export default defineConfig(({command}) => ({
  plugins: [
    catalogSchemes(),
    {
      name: 'astryx-css-layer-order',
      transformIndexHtml() {
        return [
          {
            tag: 'style',
            children:
              '@layer reset, priority1, priority2, priority3, priority4, priority5, priority6, priority7, priority8, priority9, astryx-theme;',
            injectTo: 'head-prepend',
          },
        ];
      },
    },
    stylex.vite({
      dev: process.env.NODE_ENV === 'development',
      runtimeInjection: false,
      treeshakeCompensation: true,
      useCSSLayers: true,
      debugFilePath: filePath => path.relative(__dirname, filePath),
      unstable_moduleResolution: {
        type: 'commonJS',
        rootDir: __dirname,
      },
      lightningcssOptions: {
        targets: lightningcssTargets,
      },
    }),
    react(),
  ],
  resolve: {
    alias: {
      '@astryxdesign/core/theme/tokens.stylex': path.resolve(
        __dirname,
        'node_modules/@astryxdesign/core/src/theme/tokens.stylex.ts',
      ),
      '@astryxdesign/core/astryx.css': path.resolve(
        __dirname,
        'node_modules/@astryxdesign/core/dist/astryx.css',
      ),
      // Dev serves Astryx from its source; the build serves the prebuilt dist.
      //
      // Compiling the package's StyleX source on every *build* cost ~4 s of a
      // ~5 s build and emitted a second copy of the CSS the prebuilt
      // `dist/astryx.css` already ships (~130 kB of duplicates), so builds let
      // the package's own `exports` map resolve each subpath to `dist/` — the
      // same files `tsc` checks the app against.
      //
      // Dev keeps the source path: it is the rendering this UI was tuned
      // against, and — measured — dev on it matches a production build exactly.
      // Dev on the prebuilt pair does not (8 px taller transcript rows, 1 px
      // larger icons): the prebuilt CSS is not byte-equal to what this path
      // compiles — it also emits unlayered rules, and uses layers
      // priority1…priority5 where the source path uses priority1…priority10.
      // `astryx_source_in_dev: false` in src/config.json therefore inspects the
      // prebuilt path rather than previewing a build; use
      // `npm run build && npm run preview` for that.
      ...(command === 'serve' && appConfig.astryx_source_in_dev
        ? {'@astryxdesign/core': path.resolve(__dirname, 'node_modules/@astryxdesign/core/src')}
        : {}),
    },
  },
  optimizeDeps: {
    exclude: ['@astryxdesign/core', '@astryxdesign/theme-neutral'],
  },
  build: {
    chunkSizeWarningLimit: appConfig.chunk_size_warning_limit_kb,
    rolldownOptions: {
      output: {
        // A single 800 kB entry chunk means the whole app — including libraries
        // that change far less often — is re-downloaded on every app edit, and
        // it trips the size warning for no reason the code can act on. Splitting
        // the vendors keeps the app chunk small and the libraries cacheable.
        codeSplitting: {
          groups: [
            {name: 'vendor-react', test: /node_modules[\\/](react|react-dom|scheduler)[\\/]/},
            {name: 'vendor-astryx', test: /node_modules[\\/]@astryxdesign[\\/]/},
            // Only the lazy catalog editor imports these; a shared group would load them at startup.
            {name: 'vendor-editor', test: /node_modules[\\/](@codemirror|@lezer|ajv|fast-deep-equal|fast-uri|json-schema-traverse|require-from-string|style-mod|w3c-keyname|crelt|@marijn)[\\/]/},
            {name: 'vendor', test: /node_modules[\\/]/},
          ],
        },
      },
    },
  },
  server: {
    proxy: {
      [appConfig.api_base_path]: appConfig.dev_api_proxy_target,
    },
  },
}));
