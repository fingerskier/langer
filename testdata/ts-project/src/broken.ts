interface Widget {
  id: string;
}

const widget: Widget = { id: "w1" };

// Deliberate type error: TS2339. Kept out of the reference graph on purpose.
export const brokenValue = widget.missingProp;
