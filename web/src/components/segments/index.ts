export { SegmentBuilder } from './SegmentBuilder';
export type {
  FieldType,
  ConditionType,
  LogicOperator,
  Operator,
  ConditionBuilder,
  ConditionGroupBuilder,
  ContactField,
  OperatorMeta,
  SegmentPreview,
} from './SegmentBuilder';

// SegmentDetail was deleted in PAGE_VERSION_DASHBOARD = 1.0 (2026-05-08).
// It was only used by the deleted SegmentsPage; the live segments surface is
// `SegmentBuilder` mounted via the Lists & Segments tab.
