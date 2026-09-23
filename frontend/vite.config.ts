import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {defineConfig} from 'vite';
import react from '@vitejs/plugin-react';
import stylex from '@stylexjs/unplugin';
import appConfig from './src/config.json' with {type: 'json'};

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const lightningcssTargets = {
  chrome: appConfig.browser_targets.chrome_major << 16,
  firefox: appConfig.browser_targets.firefox_major << 16,
  safari: (appConfig.browser_targets.safari_major << 16) | (appConfig.browser_targets.safari_minor << 8),
};

export default defineConfig({
  plugins: [
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
      '@astryxdesign/core': path.resolve(
        __dirname,
        'node_modules/@astryxdesign/core/src',
      ),
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
        advancedChunks: {
          groups: [
            {name: 'vendor-react', test: /node_modules[\\/](react|react-dom|scheduler)[\\/]/},
            {name: 'vendor-astryx', test: /node_modules[\\/]@astryxdesign[\\/]/},
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
});
