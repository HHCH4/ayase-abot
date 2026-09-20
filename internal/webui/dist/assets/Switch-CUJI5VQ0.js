import{v as Y,a8 as a,aw as L,a6 as A,a7 as l,a9 as G,d as ve,x as Q,g as u,c as m,a as p,a0 as r,z as n,e as q,y as J,A as ge,r as E,B as we,n as P,aX as me,E as pe,az as ye,aY as ke,ae as v,G as xe}from"./index-kBDeoDtB.js";import{i as H,r as g,a as Se,b as M,p as I,d as s}from"./use-message-BySYH7VW.js";import{a as ze}from"./Space-M4RG_evG.js";var _e=Y("switch",`
 height: var(--n-height);
 min-width: var(--n-width);
 vertical-align: middle;
 user-select: none;
 -webkit-user-select: none;
 display: inline-flex;
 outline: none;
 justify-content: center;
 align-items: center;
`,[a("children-placeholder",`
 height: var(--n-rail-height);
 display: flex;
 flex-direction: column;
 overflow: hidden;
 pointer-events: none;
 visibility: hidden;
 `),a("rail-placeholder",`
 display: flex;
 flex-wrap: none;
 `),a("button-placeholder",`
 width: calc(1.75 * var(--n-rail-height));
 height: var(--n-rail-height);
 `),Y("base-loading",`
 position: absolute;
 top: 50%;
 left: 50%;
 transform: translateX(-50%) translateY(-50%);
 font-size: calc(var(--n-button-width) - 4px);
 color: var(--n-loading-color);
 transition: color .3s var(--n-bezier);
 `,[L({left:"50%",top:"50%",originalTransform:"translateX(-50%) translateY(-50%)"})]),a("checked, unchecked",`
 transition: color .3s var(--n-bezier);
 color: var(--n-text-color);
 box-sizing: border-box;
 position: absolute;
 white-space: nowrap;
 top: 0;
 bottom: 0;
 display: flex;
 align-items: center;
 line-height: 1;
 `),a("checked",`
 right: 0;
 padding-right: calc(1.25 * var(--n-rail-height) - var(--n-offset));
 `),a("unchecked",`
 left: 0;
 justify-content: flex-end;
 padding-left: calc(1.25 * var(--n-rail-height) - var(--n-offset));
 `),A("&:focus",[a("rail",`
 box-shadow: var(--n-box-shadow-focus);
 `)]),l("round",[a("rail","border-radius: calc(var(--n-rail-height) / 2);",[a("button","border-radius: calc(var(--n-button-height) / 2);")])]),G("disabled",[G("icon",[l("rubber-band",[l("pressed",[a("rail",[a("button","max-width: var(--n-button-width-pressed);")])]),a("rail",[A("&:active",[a("button","max-width: var(--n-button-width-pressed);")])]),l("active",[l("pressed",[a("rail",[a("button","left: calc(100% - var(--n-offset) - var(--n-button-width-pressed));")])]),a("rail",[A("&:active",[a("button","left: calc(100% - var(--n-offset) - var(--n-button-width-pressed));")])])])])])]),l("active",[a("rail",[a("button","left: calc(100% - var(--n-button-width) - var(--n-offset))")])]),a("rail",`
 overflow: hidden;
 height: var(--n-rail-height);
 min-width: var(--n-rail-width);
 border-radius: var(--n-rail-border-radius);
 cursor: pointer;
 position: relative;
 transition:
 opacity .3s var(--n-bezier),
 background .3s var(--n-bezier),
 box-shadow .3s var(--n-bezier);
 background-color: var(--n-rail-color);
 `,[a("button-icon",`
 color: var(--n-icon-color);
 transition: color .3s var(--n-bezier);
 font-size: calc(var(--n-button-height) - 4px);
 position: absolute;
 left: 0;
 right: 0;
 top: 0;
 bottom: 0;
 display: flex;
 justify-content: center;
 align-items: center;
 line-height: 1;
 `,[L()]),a("button",`
 align-items: center; 
 top: var(--n-offset);
 left: var(--n-offset);
 height: var(--n-button-height);
 width: var(--n-button-width-pressed);
 max-width: var(--n-button-width);
 border-radius: var(--n-button-border-radius);
 background-color: var(--n-button-color);
 box-shadow: var(--n-button-box-shadow);
 box-sizing: border-box;
 cursor: inherit;
 content: "";
 position: absolute;
 transition:
 background-color .3s var(--n-bezier),
 left .3s var(--n-bezier),
 opacity .3s var(--n-bezier),
 max-width .3s var(--n-bezier),
 box-shadow .3s var(--n-bezier);
 `)]),l("active",[a("rail","background-color: var(--n-rail-color-active);")]),l("loading",[a("rail",`
 cursor: wait;
 `)]),l("disabled",[a("rail",`
 cursor: not-allowed;
 opacity: .5;
 `)])]);const $e=["aria-checked","tabindex","onClick","onFocus","onBlur","onKeyup","onKeydown"],Be={...Q.props,size:String,value:{type:[String,Number,Boolean],default:void 0},loading:Boolean,defaultValue:{type:[String,Number,Boolean],default:!1},disabled:{type:Boolean,default:void 0},round:{type:Boolean,default:!0},"onUpdate:value":[Function,Array],onUpdateValue:[Function,Array],checkedValue:{type:[String,Number,Boolean],default:!0},uncheckedValue:{type:[String,Number,Boolean],default:!1},railStyle:Function,rubberBand:{type:Boolean,default:!0},spinProps:Object,onChange:[Function,Array]};let _;var Fe=ve({name:"Switch",props:Be,slots:Object,setup(e){_===void 0&&(typeof CSS<"u"?typeof CSS.supports<"u"?_=CSS.supports("width","max(1px)"):_=!1:_=!0);const{mergedClsPrefixRef:$,inlineThemeDisabled:y,mergedComponentPropsRef:T}=ge(e),K=Q("Switch","-switch",_e,ke,e,$),w=Se(e,{mergedSize(t){if(e.size!==void 0)return e.size;if(t)return t.mergedSize.value;const b=T?.value?.Switch?.size;return b||"medium"}}),{mergedSizeRef:k,mergedDisabledRef:h}=w,x=E(e.defaultValue),B=xe(e,"value"),f=ze(B,x),C=P(()=>f.value===e.checkedValue),i=E(!1),o=E(!1),S=P(()=>{const{railStyle:t}=e;if(t)return t({focused:o.value,checked:C.value})});function R(t){const{"onUpdate:value":b,onChange:V,onUpdateValue:F}=e,{nTriggerFormInput:W,nTriggerFormChange:D}=w;b&&M(b,t),F&&M(F,t),V&&M(V,t),x.value=t,W(),D()}function Z(){const{nTriggerFormFocus:t}=w;t()}function ee(){const{nTriggerFormBlur:t}=w;t()}function te(){e.loading||h.value||(f.value!==e.checkedValue?R(e.checkedValue):R(e.uncheckedValue))}function ae(){o.value=!0,Z()}function ie(){o.value=!1,ee(),i.value=!1}function ne(t){e.loading||h.value||t.key===" "&&(f.value!==e.checkedValue?R(e.checkedValue):R(e.uncheckedValue),i.value=!1)}function oe(t){e.loading||h.value||t.key===" "&&(t.preventDefault(),i.value=!0)}const O=P(()=>{const{value:t}=k,{self:{opacityDisabled:b,railColor:V,railColorActive:F,buttonBoxShadow:W,buttonColor:D,boxShadowFocus:re,loadingColor:le,textColor:se,iconColor:ce,[v("buttonHeight",t)]:c,[v("buttonWidth",t)]:de,[v("buttonWidthPressed",t)]:ue,[v("railHeight",t)]:d,[v("railWidth",t)]:z,[v("railBorderRadius",t)]:he,[v("buttonBorderRadius",t)]:fe},common:{cubicBezierEaseInOut:be}}=K.value;let N,U,j;return _?(N=`calc((${d} - ${c}) / 2)`,U=`max(${d}, ${c})`,j=`max(${z}, calc(${z} + ${c} - ${d}))`):(N=I((s(d)-s(c))/2),U=I(Math.max(s(d),s(c))),j=s(d)>s(c)?z:I(s(z)+s(c)-s(d))),{"--n-bezier":be,"--n-button-border-radius":fe,"--n-button-box-shadow":W,"--n-button-color":D,"--n-button-width":de,"--n-button-width-pressed":ue,"--n-button-height":c,"--n-height":U,"--n-offset":N,"--n-opacity-disabled":b,"--n-rail-border-radius":he,"--n-rail-color":V,"--n-rail-color-active":F,"--n-rail-height":d,"--n-rail-width":z,"--n-width":j,"--n-box-shadow-focus":re,"--n-loading-color":le,"--n-text-color":se,"--n-icon-color":ce}}),X=y?we("switch",P(()=>k.value[0]),O,e):void 0;return{handleClick:te,handleBlur:ie,handleFocus:ae,handleKeyup:ne,handleKeydown:oe,mergedRailStyle:S,pressed:i,mergedClsPrefix:$,mergedValue:f,checked:C,mergedDisabled:h,cssVars:y?void 0:O,themeClass:X?.themeClass,onRender:X?.onRender}},render(){const{mergedClsPrefix:e,mergedDisabled:$,checked:y,mergedRailStyle:T,onRender:K,$slots:w}=this;K?.();const{checked:k,unchecked:h,icon:x,"checked-icon":B,"unchecked-icon":f}=w,C=!(H(x)&&H(B)&&H(f));return u(),m("div",{role:"switch","aria-checked":y,class:n([`${e}-switch`,this.themeClass,C&&`${e}-switch--icon`,y&&`${e}-switch--active`,$&&`${e}-switch--disabled`,this.round&&`${e}-switch--round`,this.loading&&`${e}-switch--loading`,this.pressed&&`${e}-switch--pressed`,this.rubberBand&&`${e}-switch--rubber-band`]),tabindex:this.mergedDisabled?void 0:0,style:J(this.cssVars),onClick:this.handleClick,onFocus:this.handleFocus,onBlur:this.handleBlur,onKeyup:this.handleKeyup,onKeydown:this.handleKeydown},[p("div",{class:n(`${e}-switch__rail`),"aria-hidden":"true",style:J(T)},[r(()=>g(k,i=>g(h,o=>i||o?(u(),m("div",{key:4,"aria-hidden":!0,class:n(`${e}-switch__children-placeholder`)},[p("div",{class:n(`${e}-switch__rail-placeholder`)},[p("div",{class:n(`${e}-switch__button-placeholder`)},null,2),r(()=>i)],2),p("div",{class:n(`${e}-switch__rail-placeholder`)},[p("div",{class:n(`${e}-switch__button-placeholder`)},null,2),r(()=>o)],2)],2)):null))),p("div",{class:n(`${e}-switch__button`)},[r(()=>g(x,i=>g(B,o=>g(f,S=>(u(),q(ye,null,{default:()=>this.loading?(u(),q(me,pe({key:"loading",clsPrefix:e,strokeWidth:20},this.spinProps),null,16,["clsPrefix"])):this.checked&&(o||i)?(u(),m("div",{class:n(`${e}-switch__button-icon`),key:o?"checked-icon":"icon"},[r(()=>o||i)],2)):!this.checked&&(S||i)?(u(),m("div",{class:n(`${e}-switch__button-icon`),key:S?"unchecked-icon":"icon"},[r(()=>S||i)],2)):null},1024)))))),r(()=>g(k,i=>i&&(u(),m("div",{key:"checked",class:n(`${e}-switch__checked`)},[r(()=>i)],2)))),r(()=>g(h,i=>i&&(u(),m("div",{key:"unchecked",class:n(`${e}-switch__unchecked`)},[r(()=>i)],2))))],2)],6)],46,$e)}});export{Fe as S};
