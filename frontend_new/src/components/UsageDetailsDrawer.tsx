import {Heading, HStack, Text, VStack} from '@astryxdesign/core';
import {ChatComposerDrawer} from '@astryxdesign/core/Chat';
import type {SessionUsageSummary} from '../api';

interface UsageDetailsDrawerProps {
  sessionName?: string;
  usage?: SessionUsageSummary | null;
}

interface TokenValues {
  input_tokens?: number;
  output_tokens?: number;
  total_tokens?: number;
  cached_input_tokens?: number;
}

function formatTokenCount(value: unknown): string | undefined {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) {
    return undefined;
  }
  return new Intl.NumberFormat().format(value);
}

function metricRows(values: TokenValues) {
  return [
    {label: 'Input tokens', value: formatTokenCount(values.input_tokens)},
    {label: 'Output tokens', value: formatTokenCount(values.output_tokens)},
    {label: 'Total tokens', value: formatTokenCount(values.total_tokens)},
    {
      label: 'Cached input tokens',
      value: formatTokenCount(values.cached_input_tokens),
    },
  ].filter((metric): metric is {label: string; value: string} => metric.value !== undefined);
}

export function UsageDetailsDrawer({
  sessionName = 'Current session',
  usage,
}: UsageDetailsDrawerProps) {
  const currentMetrics = usage?.current ? metricRows(usage.current) : [];
  const totalMetrics = usage ? metricRows(usage) : [];
  const completedResponses = formatTokenCount(usage?.completed_responses);
  const turnCount = formatTokenCount(usage?.turn_count);
  const responsesWithUsage = formatTokenCount(usage?.responses_with_usage);
  const currentTurn = formatTokenCount(usage?.current?.turn);
  const currentStep = formatTokenCount(usage?.current?.step);

  return (
    <ChatComposerDrawer>
      <VStack gap={4} padding={4}>
        <VStack gap={0.5}>
          <Heading level={2}>Usage details</Heading>
          <Text type="supporting" color="secondary">{sessionName}</Text>
          {completedResponses !== undefined && turnCount !== undefined && (
            <Text type="supporting" color="secondary">
              {completedResponses} completed model responses across {turnCount} user turns.
            </Text>
          )}
        </VStack>

        {usage?.current && (
          <VStack gap={3}>
            <VStack gap={0.5}>
              <Heading level={3}>Latest response</Heading>
              {currentTurn !== undefined && currentStep !== undefined && (
                <Text type="supporting" color="secondary">
                  Turn {currentTurn} · Step {currentStep}
                </Text>
              )}
            </VStack>
            {currentMetrics.length > 0 ? (
              <VStack gap={3}>
                {currentMetrics.map(metric => (
                  <HStack key={`current-${metric.label}`} align="center" justify="between" gap={3}>
                    <Text type="label">{metric.label}</Text>
                    <Text type="label" weight="semibold">{metric.value}</Text>
                  </HStack>
                ))}
              </VStack>
            ) : (
              <Text type="supporting" color="secondary">
                Usage data was not provided for the latest response.
              </Text>
            )}
          </VStack>
        )}

        {totalMetrics.length > 0 ? (
          <VStack gap={3}>
            <Heading level={3}>Session totals</Heading>
            {totalMetrics.map(metric => (
              <HStack key={`total-${metric.label}`} align="center" justify="between" gap={3}>
                <Text type="label">{metric.label}</Text>
                <Text type="label" weight="semibold">{metric.value}</Text>
              </HStack>
            ))}
            {responsesWithUsage !== undefined && completedResponses !== undefined &&
              responsesWithUsage !== completedResponses && (
                <Text type="supporting" color="secondary">
                  Usage was recorded for {responsesWithUsage} of {completedResponses} completed responses.
                </Text>
              )}
            <Text type="supporting" color="secondary">
              Cached input tokens are included in input tokens.
            </Text>
          </VStack>
        ) : (
          <Text type="supporting" color="secondary" role="status" aria-live="polite">
            {usage && usage.completed_responses > 0
              ? 'Usage data was not provided for this session’s completed responses.'
              : 'Usage details will appear after the first completed response.'}
          </Text>
        )}

        {usage?.history_partial && (
          <Text type="supporting" color="secondary">
            Earlier usage may be missing because the transcript was compacted before its usage cache was available.
          </Text>
        )}
      </VStack>
    </ChatComposerDrawer>
  );
}
