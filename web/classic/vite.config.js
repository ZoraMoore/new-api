/*
Copyright (C) 2025 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/

import react from '@vitejs/plugin-react';
import { defineConfig, transformWithEsbuild } from 'vite';
import pkg from '@douyinfe/vite-plugin-semi';
import path from 'path';
import fs from 'fs';
import { pathToFileURL } from 'url';
import { codeInspectorPlugin } from 'code-inspector-plugin';
const { vitePluginSemi } = pkg;

// Sass importer that resolves Webpack-style `~package` imports to node_modules.
const tildeImporter = {
  canonicalize(url) {
    if (!url.startsWith('~')) return null;
    const stripped = url.slice(1);
    const candidates = [stripped, `${stripped}/index`].flatMap((p) => [
      `${p}.scss`,
      `${p}.sass`,
      `${p}.css`,
      `_${path.basename(p)}.scss`.replace(path.basename(p), '') + path.basename(p).replace(/^_?/, '_') + '.scss',
      p,
    ]);
    for (const cand of [stripped, ...candidates]) {
      const abs = path.resolve(process.cwd(), 'node_modules', cand);
      if (fs.existsSync(abs) && fs.statSync(abs).isFile()) {
        return pathToFileURL(abs);
      }
    }
    return null;
  },
  load(canonicalUrl) {
    const filePath = canonicalUrl.pathname;
    const decoded = decodeURIComponent(
      process.platform === 'win32' && filePath.startsWith('/') ? filePath.slice(1) : filePath
    );
    return {
      contents: fs.readFileSync(decoded, 'utf8'),
      syntax: decoded.endsWith('.sass') ? 'indented' : 'scss',
    };
  },
};

// https://vitejs.dev/config/
export default defineConfig({
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  css: {
    preprocessorOptions: {
      scss: {
        importers: [tildeImporter],
      },
    },
  },
  plugins: [
    codeInspectorPlugin({
      bundler: 'vite',
    }),
    {
      name: 'treat-js-files-as-jsx',
      async transform(code, id) {
        if (!/src\/.*\.js$/.test(id)) {
          return null;
        }

        // Use the exposed transform from vite, instead of directly
        // transforming with esbuild
        return transformWithEsbuild(code, id, {
          loader: 'jsx',
          jsx: 'automatic',
        });
      },
    },
    react(),
    vitePluginSemi({
      cssLayer: true,
    }),
  ],
  optimizeDeps: {
    force: true,
    esbuildOptions: {
      loader: {
        '.js': 'jsx',
        '.json': 'json',
      },
    },
  },
  build: {
    rollupOptions: {
      output: {
        manualChunks: {
          'react-core': ['react', 'react-dom', 'react-router-dom'],
          'semi-ui': ['@douyinfe/semi-icons', '@douyinfe/semi-ui'],
          tools: ['axios', 'history', 'marked'],
          'react-components': [
            'react-dropzone',
            'react-fireworks',
            'react-telegram-login',
            'react-toastify',
            'react-turnstile',
          ],
          i18n: [
            'i18next',
            'react-i18next',
            'i18next-browser-languagedetector',
          ],
        },
      },
    },
  },
  server: {
    host: '0.0.0.0',
    proxy: {
      '/api': {
        target: 'http://localhost:3000',
        changeOrigin: true,
      },
      '/mj': {
        target: 'http://localhost:3000',
        changeOrigin: true,
      },
      '/pg': {
        target: 'http://localhost:3000',
        changeOrigin: true,
      },
    },
  },
});
