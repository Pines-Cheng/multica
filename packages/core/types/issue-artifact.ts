export interface IssueArtifact {
  key: string;
  title: string;
  url: string;
  kind: string;
}

export type IssueArtifactInput = Omit<IssueArtifact, "key" | "kind"> & { kind?: string };

export interface IssueArtifactsResponse {
  artifacts: IssueArtifact[];
  issue_revision?: number;
}
