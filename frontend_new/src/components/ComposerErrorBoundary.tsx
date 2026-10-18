import {Component, type ErrorInfo, type ReactNode} from 'react';
import {Button, HStack, Text, VStack} from '@astryxdesign/core';

/**
 * Contains a composer crash instead of losing the whole app.
 *
 * React answers an error thrown while it patches the DOM — report 0005: a
 * deletion pass meeting a node the composer's own edits already moved — by
 * unmounting the root, which leaves a blank page and a reload as the only way
 * back. A boundary here turns that into a composer-sized failure: the
 * conversation, the sidebar and the draft store survive, and the composer can be
 * restored in place. Restoring remounts the input, which reads its draft back
 * from the draft store; a badge that was on the draft comes back as its text.
 */
type ComposerErrorBoundaryProps = {
  children: ReactNode;
  onError?: (error: unknown) => void;
};

type ComposerErrorBoundaryState = {
  error: unknown;
};

export class ComposerErrorBoundary extends Component<
  ComposerErrorBoundaryProps,
  ComposerErrorBoundaryState
> {
  state: ComposerErrorBoundaryState = {error: null};

  static getDerivedStateFromError(error: unknown): ComposerErrorBoundaryState {
    return {error};
  }

  componentDidCatch(error: unknown, info: ErrorInfo): void {
    console.error('The composer failed; the app kept running.', error, info.componentStack);
    this.props.onError?.(error);
  }

  private readonly restore = (): void => {
    this.setState({error: null});
  };

  render(): ReactNode {
    if (this.state.error === null) {
      return this.props.children;
    }
    return (
      <VStack gap={2} width="100%">
        <Text type="supporting" color="secondary">
          The composer hit an error while the app was updating it, so it was
          replaced instead of the whole page. Your draft is kept.
        </Text>
        <HStack align="center" gap={2}>
          <Button label="Restore composer" variant="secondary" size="sm" onClick={this.restore} />
        </HStack>
      </VStack>
    );
  }
}
