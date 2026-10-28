export interface Pair {
  fg: string;
  bg: string[];
  kind: string;
}
export declare const required: { text: number; large: number; ui: number };
export declare function parseTokens(css: string): Record<string, string>;
export declare function contrastRatio(a: string, b: string): number;
export declare function checkThemes(
  themes: Record<string, Record<string, string>>,
  pairs: Pair[],
): { checked: Record<string, number>; failures: string[] };
export declare function main(dir: string): number;
