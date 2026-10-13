/**
 * @file LazyLoadBoundary.tsx
 * @input A lazily loaded surface and a name for it
 * @output The surface, or a short notice with a retry when its code could not load
 * @position Keeps a failed chunk load from unmounting the whole app
 */
import {Component, type ReactNode} from 'react';
import {Banner, Button} from '@astryxdesign/core';

type LazyLoadBoundaryProps = {
  name: string;
  children: ReactNode;
};

type LazyLoadBoundaryState = {
  error: unknown;
};

export class LazyLoadBoundary extends Component<LazyLoadBoundaryProps, LazyLoadBoundaryState> {
  state: LazyLoadBoundaryState = {error: null};

  static getDerivedStateFromError(error: unknown): LazyLoadBoundaryState {
    return {error};
  }

  private readonly retry = (): void => {
    this.setState({error: null});
  };

  render(): ReactNode {
    if (this.state.error === null) return this.props.children;
    const detail = this.state.error instanceof Error ? this.state.error.message : String(this.state.error);
    return (
      <Banner
        status="error"
        title={`Could not load ${this.props.name}`}
        description={detail}
        collapsible={false}
        endContent={<Button label="Retry" size="sm" onClick={this.retry} />}
      />
    );
  }
}
