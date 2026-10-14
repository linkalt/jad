/**
 * @file CatalogSchemaTab.tsx
 * @input The catalog kind being edited
 * @output The fields its sample/schemes schema allows, with type, default and description
 * @position The Schema tab of the catalog editor; the schemes load on first use, as for the validator
 */
import {useEffect, useMemo, useState} from 'react';

import {Banner, HStack, Text, Token, VStack} from '@astryxdesign/core';
import {Divider} from '@astryxdesign/core/Divider';

import {CATALOG_SCHEMES, type CatalogKind} from '../catalog-validation';
import {schemaFields} from '../catalog-schema-view';

type Loaded = {kind: CatalogKind; schema: unknown} | {kind: CatalogKind; error: string};

export function CatalogSchemaTab({kind}: {kind: CatalogKind}) {
  const [loaded, setLoaded] = useState<Loaded | null>(null);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const {default: schemes} = await import('virtual:catalog-schemes');
        if (!cancelled) setLoaded({kind, schema: (schemes as Record<string, unknown>)[CATALOG_SCHEMES[kind]]});
      } catch (caught) {
        if (!cancelled) setLoaded({kind, error: caught instanceof Error ? caught.message : String(caught)});
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [kind]);

  const current = loaded !== null && loaded.kind === kind ? loaded : null;
  const fields = useMemo(() => (current !== null && 'schema' in current ? schemaFields(current.schema) : []), [current]);

  if (current === null) return <Text type="supporting" color="secondary">Loading the schema…</Text>;
  if ('error' in current) {
    return <Banner status="warning" title="The schema could not load" description={current.error} collapsible={false} />;
  }
  if (fields.length === 0) return <Text type="supporting" color="secondary">This schema lists no fields.</Text>;

  return (
    <VStack gap={0} isScrollable height="100%">
      {fields.map((field, index) => (
        <VStack key={field.name} gap={1}>
          {index > 0 && <Divider />}
          <VStack gap={1} paddingBlock={2}>
          <HStack gap={2} align="center">
            <Text type="label">{field.name}</Text>
            {field.type !== '' && <Token label={field.type} size="sm" />}
            {field.isRequired && <Token label="required" size="sm" />}
            {field.defaultValue !== undefined && (
              <Text type="supporting" color="secondary">default {field.defaultValue}</Text>
            )}
          </HStack>
          {field.description !== '' && <Text type="supporting" color="secondary">{field.description}</Text>}
          </VStack>
        </VStack>
      ))}
    </VStack>
  );
}
