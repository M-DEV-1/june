#!/usr/bin/env npx tsx
import React from 'react';
import {render} from 'ink';
import Ora from './components/Ora.js';
import { GlobalErrorBoundary } from './components/ErrorBoundaries.js';

render(
    <GlobalErrorBoundary>
        <Ora />
    </GlobalErrorBoundary>
);
