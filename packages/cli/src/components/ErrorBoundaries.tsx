import React, { Component, ErrorInfo, ReactNode } from 'react';
import { Box, Text } from 'ink';

interface Props {
  children?: ReactNode;
}

interface State {
  hasError: boolean;
  error: Error | null;
}

export class GlobalErrorBoundary extends Component<Props, State> {
  public state: State = {
    hasError: false,
    error: null
  };

  public static getDerivedStateFromError(error: Error): State {
    return { hasError: true, error };
  }

  public componentDidCatch(error: Error, errorInfo: ErrorInfo) {
    // In a real production app, we would log this to a service
    // console.error("Uncaught error:", error, errorInfo);
  }

  public render() {
    if (this.state.hasError) {
      return (
        <Box flexDirection="column" borderStyle="double" borderColor="red" padding={1}>
          <Text bold color="red"> ⚠ CRITICAL SYSTEM FAILURE </Text>
          <Box marginTop={1}>
            <Text color="yellow">ORA has encountered an unrecoverable error and needs to shut down.</Text>
          </Box>
          <Box marginTop={1} padding={1} borderStyle="single" borderColor="gray">
            <Text color="gray" dimColor>{this.state.error?.stack || this.state.error?.message}</Text>
          </Box>
          <Box marginTop={1}>
            <Text color="cyan">Please restart the application. If this persists, check your neural link.</Text>
          </Box>
        </Box>
      );
    }

    return this.props.children;
  }
}
