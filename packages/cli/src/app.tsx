#!/usr/bin/env npx tsx
import React from 'react';
import {render} from 'ink';
import dotenv from 'dotenv';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import Ora from './components/Ora.js';
import { GlobalErrorBoundary } from './components/ErrorBoundaries.js';

// Initialize environment from workspace root
const __dirname = path.dirname(fileURLToPath(import.meta.url));
dotenv.config({ path: path.resolve(__dirname, '../../../.env'), override: true });

render(
    <GlobalErrorBoundary>
        <Ora />
    </GlobalErrorBoundary>
);
